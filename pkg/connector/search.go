package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var reUserID = regexp.MustCompile(`^@[^:\s]+:\S+$`)

// ResolveIdentifier finds a person there by Matrix ID (@name:server). With createChat it opens the DM
// the account already has with them, or makes a new encrypted one.
func (mc *MatrixClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	user := id.UserID(strings.TrimSpace(identifier))
	if !reUserID.MatchString(string(user)) {
		return nil, bridgev2.WrapRespErrManual(errors.New("a Matrix ID looks like @name:server"), mautrix.MInvalidParam.ErrCode, http.StatusBadRequest)
	}
	if user == mc.meta().UserID {
		return nil, bridgev2.WrapRespErrManual(errors.New("that is this account itself"), mautrix.MInvalidParam.ErrCode, http.StatusBadRequest)
	}
	client, err := mc.remote()
	if err != nil {
		return nil, err
	}
	name := string(user)
	info := &bridgev2.UserInfo{Identifiers: []string{"matrix:" + string(user)}}
	profile, err := client.GetProfile(ctx, user)
	switch {
	case errors.Is(err, mautrix.MNotFound):
		return nil, bridgev2.WrapRespErrManual(fmt.Errorf("%s: %w", user, err), mautrix.MNotFound.ErrCode, http.StatusNotFound)
	case err == nil:
		name = cmpOr(profile.DisplayName, name)
		info.Avatar = mc.avatar(profile.AvatarURL.CUString())
		// Many servers show profiles only to people who share a room; any other refusal is no answer.
	}
	info.Name = ptr.Ptr(name)
	resp := &bridgev2.ResolveIdentifierResponse{UserID: userID(user), UserInfo: info}
	if resp.Ghost, err = mc.Main.Bridge.GetGhostByID(ctx, userID(user)); err != nil {
		return nil, err
	}
	if !createChat {
		return resp, nil
	}
	room, err := mc.directRoom(ctx, client, user)
	if err != nil {
		return nil, err
	}
	key := portalKey(room, mc.UserLogin)
	chatInfo, err := mc.GetChatInfo(ctx, &bridgev2.Portal{Portal: &database.Portal{PortalKey: key}})
	if err != nil {
		return nil, err
	}
	resp.Chat = &bridgev2.CreateChatResponse{PortalKey: key, PortalInfo: chatInfo}
	return resp, nil
}

// directRoom is a DM with the person there: one the account already has (from another app too), else a
// new one, encrypted and remembered in m.direct like any client does.
func (mc *MatrixClient) directRoom(ctx context.Context, client *mautrix.Client, user id.UserID) (id.RoomID, error) {
	direct := map[id.UserID][]id.RoomID{}
	if err := client.GetAccountData(ctx, event.AccountDataDirectChats.Type, &direct); err != nil && !errors.Is(err, mautrix.MNotFound) {
		return "", err
	}
	joined, err := client.JoinedRooms(ctx)
	if err != nil {
		return "", err
	}
	in := map[id.RoomID]bool{}
	for _, r := range joined.JoinedRooms {
		in[r] = true
	}
	for _, r := range direct[user] {
		if in[r] && !mc.isBridge(ctx, r) {
			return r, nil
		}
	}
	encryption := &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}
	created, err := client.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Preset: "trusted_private_chat", IsDirect: true, Invite: []id.UserID{user},
		InitialState: []*event.Event{{Type: event.StateEncryption, Content: event.Content{Parsed: encryption}}},
	})
	if err != nil {
		return "", err
	}
	if err = client.StateStore.SetEncryptionEvent(ctx, created.RoomID, encryption); err != nil {
		return "", err
	}
	direct[user] = append(direct[user], created.RoomID)
	if err = client.SetAccountData(ctx, event.AccountDataDirectChats.Type, direct); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to remember the new DM in m.direct")
	}
	return created.RoomID, nil
}

var reRoom = regexp.MustCompile(`^#[^:\s]+:\S+$|^![^\s]+$`)

// addProvisioning adds POST /v3/join {"room": "#alias:server"}: the account joins a room there and the
// bridge makes its portal. Rooms that are themselves bridges are left again.
func (mc *MatrixConnector) addProvisioning() {
	withProv, ok := mc.Bridge.Matrix.(bridgev2.MatrixConnectorWithProvisioning)
	if !ok {
		return
	}
	prov := withProv.GetProvisioning()
	if prov == nil {
		return
	}
	prov.GetRouter().HandleFunc("POST /v3/join", func(w http.ResponseWriter, r *http.Request) {
		login := prov.GetLoginForRequest(w, r)
		if login == nil {
			return
		}
		var body struct {
			Room string `json:"room"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		roomID, err := login.Client.(*MatrixClient).join(r.Context(), strings.TrimSpace(body.Room))
		if err != nil {
			matrix.RespondWithError(w, err, "Failed to join the room")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"room_id": roomID})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// join joins a room there by alias or ID and returns its portal's room here.
func (mc *MatrixClient) join(ctx context.Context, target string) (id.RoomID, error) {
	if !reRoom.MatchString(target) {
		return "", bridgev2.WrapRespErrManual(errors.New("a room address looks like #name:server"), mautrix.MInvalidParam.ErrCode, http.StatusBadRequest)
	}
	client, err := mc.remote()
	if err != nil {
		return "", err
	}
	joined, err := client.JoinRoom(ctx, target, nil)
	if err != nil {
		var resp mautrix.HTTPError
		if errors.As(err, &resp) && resp.RespError != nil {
			return "", bridgev2.WrapRespErrManual(fmt.Errorf("%s: %s", target, resp.RespError.Err), resp.RespError.ErrCode, http.StatusBadRequest)
		}
		return "", err
	}
	if mc.isBridge(ctx, joined.RoomID) {
		_, _ = client.LeaveRoom(ctx, joined.RoomID) // not left sitting in a room the bridge does not show
		return "", bridgev2.WrapRespErrManual(errors.New("that room is itself a bridge to another network; it is left out"),
			"IO.GITHUB.GOODER24.MATRIX.BRIDGED", http.StatusBadRequest)
	}
	portal, err := mc.Main.Bridge.GetPortalByKey(ctx, portalKey(joined.RoomID, mc.UserLogin))
	if err != nil {
		return "", err
	}
	info, err := mc.GetChatInfo(ctx, portal)
	if err != nil {
		return "", err
	}
	if err = portal.CreateMatrixRoom(ctx, mc.UserLogin, info); err != nil {
		return "", err
	}
	return portal.MXID, nil
}
