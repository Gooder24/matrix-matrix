package connector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"
)

const (
	LoginFlowPassword = "password"
	LoginFlowToken    = "token"
	stepPassword      = "io.github.gooder24.matrix.login.password"
	stepToken         = "io.github.gooder24.matrix.login.token"
	stepDone          = "io.github.gooder24.matrix.login.complete"
)

func (mc *MatrixConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Password",
		Description: "Sign in with your homeserver, username and password; the bridge adds its own device",
		ID:          LoginFlowPassword,
	}, {
		Name:        "Access token",
		Description: "For accounts without a password (like Beeper): the bridge uses the token to make its own device",
		ID:          LoginFlowToken,
	}}
}

func (mc *MatrixConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != LoginFlowPassword && flowID != LoginFlowToken {
		return nil, fmt.Errorf("unknown login flow %q", flowID)
	}
	return &MatrixLogin{Main: mc, User: user, flow: flowID}, nil
}

type MatrixLogin struct {
	Main *MatrixConnector
	User *bridgev2.User
	flow string
}

var _ bridgev2.LoginProcessUserInput = (*MatrixLogin)(nil)

func loginError(code, message string) error {
	return bridgev2.RespError{ErrCode: "IO.GITHUB.GOODER24.MATRIX." + code, Err: message, StatusCode: 400}
}

func (ml *MatrixLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	fields := []bridgev2.LoginInputDataField{{
		Type: bridgev2.LoginInputFieldTypeURL, ID: "homeserver", Name: "Homeserver",
		Description: "matrix.org or your server's address; empty takes it from the user ID", Optional: true,
	}}
	step := &bridgev2.LoginStep{Type: bridgev2.LoginStepTypeUserInput, StepID: stepPassword,
		Instructions: "Sign in to your Matrix account. The bridge adds its own device with its own encryption keys."}
	if ml.flow == LoginFlowPassword {
		fields = append(fields,
			bridgev2.LoginInputDataField{Type: bridgev2.LoginInputFieldTypeUsername, ID: "username", Name: "Username", Description: "@you:example.org, or the name alone"},
			bridgev2.LoginInputDataField{Type: bridgev2.LoginInputFieldTypePassword, ID: "password", Name: "Password"})
	} else {
		step.StepID = stepToken
		fields = append(fields, bridgev2.LoginInputDataField{Type: bridgev2.LoginInputFieldTypeToken, ID: "token", Name: "Access token",
			Description: "A session you keep; signing out of the bridge does not end it"})
	}
	step.UserInputParams = &bridgev2.LoginUserInputParams{Fields: fields}
	return step, nil
}

func (ml *MatrixLogin) Cancel() {}

func (ml *MatrixLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	given, username := strings.TrimSpace(input["homeserver"]), strings.TrimSpace(input["username"])
	if given == "" && strings.Contains(username, ":") {
		given = username[strings.Index(username, ":")+1:]
	}
	homeserver, err := resolveHomeserver(ctx, given)
	if err != nil {
		return nil, err
	}
	client, err := mautrix.NewClient(homeserver, "", "")
	if err != nil {
		return nil, loginError("INVALID_HOMESERVER", err.Error())
	}
	meta := &UserLoginMetadata{Homeserver: homeserver, OwnDevice: true, Since: time.Now().UnixMilli(), PickleKey: randomHex(32)}
	if ml.flow == LoginFlowPassword {
		ident := username
		if !strings.Contains(ident, ":") {
			ident = strings.TrimPrefix(ident, "@")
		}
		resp, err := client.Login(ctx, &mautrix.ReqLogin{
			Type:                     mautrix.AuthTypePassword,
			Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: ident},
			Password:                 input["password"],
			InitialDeviceDisplayName: ml.Main.Config.DeviceName,
		})
		if err != nil {
			return nil, loginError("LOGIN_FAILED", "The homeserver refused: "+errorText(err))
		}
		meta.UserID, meta.DeviceID, meta.Token = resp.UserID, resp.DeviceID, resp.AccessToken
	} else if err = ml.tokenLogin(ctx, client, strings.TrimSpace(input["token"]), meta); err != nil {
		return nil, err
	}
	return ml.complete(ctx, meta)
}

// tokenLogin: two key sets on one device break encryption for both, so the bridge asks the given session for a
// one-time login token and signs in a device of its own. Only a session without keys may be used as it is.
func (ml *MatrixLogin) tokenLogin(ctx context.Context, client *mautrix.Client, token string, meta *UserLoginMetadata) error {
	client.AccessToken = token
	who, err := client.Whoami(ctx)
	if err != nil {
		return loginError("LOGIN_FAILED", "The homeserver did not accept the token: "+errorText(err))
	}
	if who.DeviceID == "" {
		return loginError("NO_DEVICE", "This token has no device; the bridge needs one for encryption.")
	}
	var once struct {
		LoginToken string `json:"login_token"`
	}
	_, err = client.MakeRequest(ctx, http.MethodPost, client.BuildClientURL("v1", "login", "get_token"), struct{}{}, &once)
	if err == nil && once.LoginToken != "" {
		fresh, _ := mautrix.NewClient(client.HomeserverURL.String(), "", "")
		resp, err := fresh.Login(ctx, &mautrix.ReqLogin{Type: mautrix.AuthTypeToken, Token: once.LoginToken, InitialDeviceDisplayName: ml.Main.Config.DeviceName})
		if err != nil {
			return loginError("LOGIN_FAILED", "The homeserver refused the login token: "+errorText(err))
		}
		meta.UserID, meta.DeviceID, meta.Token = resp.UserID, resp.DeviceID, resp.AccessToken
		return nil
	}
	keys, err := client.QueryKeys(ctx, &mautrix.ReqQueryKeys{DeviceKeys: mautrix.DeviceKeysRequest{who.UserID: {who.DeviceID}}})
	if err != nil {
		return loginError("LOGIN_FAILED", "Could not check the session's keys: "+errorText(err))
	}
	if _, has := keys.DeviceKeys[who.UserID][who.DeviceID]; has {
		return loginError("DEVICE_HAS_KEYS", "This session already has its own encryption keys and the server does not let it sign in a new device. "+
			"Sign in with a password, or give the token of a new session that has not encrypted anything yet.")
	}
	meta.UserID, meta.DeviceID, meta.Token, meta.OwnDevice = who.UserID, who.DeviceID, token, false
	return nil
}

func (ml *MatrixLogin) complete(ctx context.Context, meta *UserLoginMetadata) (*bridgev2.LoginStep, error) {
	// Two sign-ins of one account at once would both replace the same login; the second waits for the first.
	lock, _ := ml.Main.signIns.LoadOrStore(loginID(meta.UserID), &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	// A sign-in that is not finished here ends the device it made there, so no session is left behind.
	if err := ctx.Err(); err != nil {
		abandon(ctx, meta)
		return nil, err
	}
	// Signing in again replaces the earlier login. Its device is ended only once the new one is saved: a save that
	// fails leaves the earlier login as it was.
	old, _ := ml.Main.Bridge.GetExistingUserLoginByID(ctx, loginID(meta.UserID))
	var oldClient *MatrixClient
	var prev *UserLoginMetadata
	if old != nil {
		oldClient, prev = old.Client.(*MatrixClient), old.Metadata.(*UserLoginMetadata)
		if oldClient.beingDeleted() {
			// Its sign-out deletes the login when it ends, and would take this one with it.
			abandon(ctx, meta)
			return nil, loginError("SIGN_OUT_RUNNING", "This account is being signed out right now. Sign in again in a moment.")
		}
		if prev.DeviceID == meta.DeviceID {
			// The same session given again (a token): its keys stay, so they must open with the same key.
			meta.PickleKey = prev.PickleKey
		}
	}
	ul, err := ml.User.NewLogin(ctx, &database.UserLogin{
		ID:            loginID(meta.UserID),
		RemoteName:    string(meta.UserID),
		RemoteProfile: status.RemoteProfile{Username: string(meta.UserID)},
		Metadata:      meta,
	}, &bridgev2.NewLoginParams{DeleteOnConflict: true})
	cleanup := context.WithoutCancel(ctx)
	if err != nil {
		abandon(cleanup, meta)
		if still, _ := ml.Main.Bridge.GetExistingUserLoginByID(cleanup, loginID(meta.UserID)); old != nil && still == old {
			// bridgev2 may have put the new login in place in memory before its save failed; the database still
			// holds the earlier one, which goes on.
			old.Metadata, old.Client = prev, oldClient
			if !oldClient.IsLoggedIn() {
				oldClient.Connect(cleanup)
			}
		} else if old != nil && prev.OwnDevice && prev.DeviceID != meta.DeviceID {
			// bridgev2 deleted the earlier login (another user of this bridge had the account) without ending its device.
			endDevice(cleanup, prev)
		}
		return nil, fmt.Errorf("failed to save the login: %w", err)
	}
	if old != nil && prev.DeviceID != meta.DeviceID {
		oldClient.retire()
		if prev.OwnDevice {
			endDevice(cleanup, prev)
		}
	}
	client := ul.Client.(*MatrixClient)
	// Keys of an earlier device of this login never come back.
	client.removeOtherKeys()
	starting := ctx
	if old != nil && prev.DeviceID != meta.DeviceID {
		starting = cleanup // the earlier device is gone: only a sign-out stops the new one now
	}
	if err = client.start(client.stoppable(starting)); err != nil { // a sign-out ends it
		client.dropDevice(cleanup)
		ul.Delete(cleanup, status.BridgeState{StateEvent: status.StateLoggedOut, Reason: "START_FAILED"}, bridgev2.DeleteOpts{LogoutRemote: false})
		return nil, loginError("START_FAILED", "Signed in, but encryption could not start: "+err.Error())
	}
	return &bridgev2.LoginStep{
		Type:           bridgev2.LoginStepTypeComplete,
		StepID:         stepDone,
		Instructions:   "Signed in as " + string(meta.UserID),
		CompleteParams: &bridgev2.LoginCompleteParams{UserLoginID: ul.ID, UserLogin: ul},
	}, nil
}

// abandon ends the device a sign-in made there when the sign-in is not kept here. A session the user gave
// (a token) stays theirs.
func abandon(ctx context.Context, meta *UserLoginMetadata) {
	if meta.OwnDevice {
		endDevice(ctx, meta)
	}
}

var reLocal = regexp.MustCompile(`^http://(127\.0\.0\.1|localhost|\[::1\])(:\d+)?(/|$)`)

// resolveHomeserver turns an address as typed into the client API's base URL, through .well-known when the
// server has one. A password never goes out unencrypted: plain http only to this machine.
func resolveHomeserver(ctx context.Context, given string) (string, error) {
	if given == "" {
		return "", loginError("INVALID_HOMESERVER", "Give the homeserver, or a full user ID like @you:example.org.")
	}
	base := given
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	base = strings.TrimRight(base, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	if u, err := url.Parse(base); err == nil {
		var wk struct {
			Homeserver struct {
				BaseURL string `json:"base_url"`
			} `json:"m.homeserver"`
		}
		if getJSON(ctx, client, u.Scheme+"://"+u.Host+"/.well-known/matrix/client", &wk) == nil && wk.Homeserver.BaseURL != "" {
			base = strings.TrimRight(wk.Homeserver.BaseURL, "/")
		}
	}
	if !strings.HasPrefix(base, "https://") && !reLocal.MatchString(base) {
		return "", loginError("INSECURE_HOMESERVER", "The homeserver must use https.")
	}
	var versions struct {
		Versions []string `json:"versions"`
	}
	if err := getJSON(ctx, client, base+"/_matrix/client/versions", &versions); err != nil || len(versions.Versions) == 0 {
		return "", loginError("INVALID_HOMESERVER", "That address does not answer like a Matrix homeserver.")
	}
	return base, nil
}

func getJSON(ctx context.Context, client *http.Client, addr string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", addr, resp.StatusCode)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(out)
}

func errorText(err error) string {
	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.RespError != nil && httpErr.RespError.Err != "" {
		return httpErr.RespError.Err
	}
	return err.Error()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
