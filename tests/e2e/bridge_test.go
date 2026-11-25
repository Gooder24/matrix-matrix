// Package e2e runs the bridge as its own process between two throwaway Tuwunel homeservers on
// 127.0.0.1: "test" (the bridge's, with its appservice and a double puppet) and "remote.test" (the other
// server, with the bridged account there and a friend who uses encryption). Nothing federates and
// nothing leaves the machine.
//
//	TUWUNEL=/path/to/tuwunel go test ./tests/e2e -v
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	server = "test"
	remote = "remote.test"
	wait   = 30 * time.Second
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func secret() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// until waits for something the bridge or a server does on its own and reports whether it happened.
func until(d time.Duration, check func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if check() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return check()
}

// ghost is the Matrix user here of a user there ("@bea:remote.test" → @matrix_bea=3aremote.test:test).
func ghost(user string) string {
	return "@matrix_" + id.EncodeUserLocalpart(strings.TrimPrefix(user, "@")) + ":" + server
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// homeserver starts a Tuwunel and returns its URL; appservices may be "".
func homeserver(t *testing.T, dir, name, regToken, appservices string) string {
	tuwunel := os.Getenv("TUWUNEL")
	port := freePort(t)
	base := filepath.Join(dir, name)
	must(t, os.MkdirAll(base, 0o700))
	conf := fmt.Sprintf("[global]\nserver_name = %q\ndatabase_path = %q\naddress = [\"127.0.0.1\"]\nport = %d\n"+
		"allow_registration = true\nregistration_token = %q\nallow_federation = false\nnew_user_displayname_suffix = \"\"\n"+
		"log = \"warn\"\nlog_colors = false\n", name, filepath.Join(base, "db"), port, regToken)
	if appservices != "" {
		conf += fmt.Sprintf("appservice_dir = %q\n", appservices)
	}
	must(t, os.WriteFile(filepath.Join(base, "tuwunel.toml"), []byte(conf), 0o600))
	logFile, _ := os.Create(filepath.Join(base, "log"))
	cmd := exec.Command(tuwunel, "-c", filepath.Join(base, "tuwunel.toml"))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	must(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	if !until(30*time.Second, func() bool {
		resp, err := http.Get(addr + "/_matrix/client/versions")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil && resp.StatusCode == 200
	}) {
		t.Fatalf("Tuwunel %s did not start", name)
	}
	return addr
}

type env struct {
	t         *testing.T
	dir       string
	c         *mx // the user here
	there     *mx // the same person's account there, without encryption, for what a second app would do
	bea       *friend
	remoteURL string
	prov      string
	provAuth  string
	bin       string
	config    string
	br        *exec.Cmd
}

func (e *env) provision(method, path string, body any, out any) error {
	data, _ := json.Marshal(body)
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, _ := http.NewRequest(method, e.prov+path+sep+"user_id="+url.QueryEscape(e.c.userID), bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+e.provAuth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func setup(t *testing.T) *env {
	if os.Getenv("TUWUNEL") == "" {
		t.Skip("TUWUNEL is not set")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "matrix-matrix")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/matrix-matrix")
	build.Stderr = os.Stderr
	must(t, build.Run())

	brPort := freePort(t)
	asToken, hsToken, regToken, provSecret := secret(), secret(), secret(), secret()
	apps := filepath.Join(dir, "appservices")
	must(t, os.Mkdir(apps, 0o700))
	registration := fmt.Sprintf(`id: matrix
url: http://127.0.0.1:%d
as_token: %s
hs_token: %s
sender_localpart: matrixbot
rate_limited: false
receive_ephemeral: true
de.sorunome.msc2409.push_ephemeral: true
namespaces:
  users:
  - regex: '^@matrixbot:%[4]s$'
    exclusive: true
  - regex: '^@matrix_.*:%[4]s$'
    exclusive: true
  - regex: '^@alex:%[4]s$'
    exclusive: false
`, brPort, asToken, hsToken, server)
	must(t, os.WriteFile(filepath.Join(apps, "matrix.yaml"), []byte(registration), 0o600))
	localURL := homeserver(t, dir, server, regToken, apps)
	remoteToken := secret()
	remoteURL := homeserver(t, dir, remote, remoteToken, "")

	c := &mx{t: t, base: localURL, events: map[string][]map[string]any{}, ephemeral: map[string][]map[string]any{}, stop: make(chan struct{})}
	c.register("alex", regToken)
	go c.syncLoop()
	t.Cleanup(func() { close(c.stop) })
	there := &mx{t: t, base: remoteURL, events: map[string][]map[string]any{}, ephemeral: map[string][]map[string]any{}, stop: make(chan struct{})}
	there.register("alex", remoteToken)
	go there.syncLoop()
	t.Cleanup(func() { close(there.stop) })
	bea := newFriend(t, dir, remoteURL, "bea", remoteToken)

	config := fmt.Sprintf(`network:
    device_name: Matrix bridge
    crypto_dir: %s
    max_file_size: 1048576
    members_at_once: 20
bridge:
    personal_filtering_spaces: false
    permissions:
        "%s": admin
    cleanup_on_logout:
        enabled: true
        manual:
            private: delete
            relayed: delete
            shared_no_users: delete
            shared_has_users: delete
matrix:
    message_status_events: true
database:
    type: sqlite3-fk-wal
    uri: file:%s?_txlock=immediate
homeserver:
    address: %s
    domain: %s
appservice:
    address: http://127.0.0.1:%d
    hostname: 127.0.0.1
    port: %d
    id: matrix
    bot:
        username: matrixbot
    as_token: %s
    hs_token: %s
    username_template: matrix_{{.}}
double_puppet:
    secrets:
        %s: as_token:%s
provisioning:
    shared_secret: %s
logging:
    min_level: debug
    writers:
    - type: stdout
      format: pretty
`, filepath.Join(dir, "crypto"), c.userID, filepath.Join(dir, "bridge.db"), localURL, server, brPort, brPort, asToken, hsToken,
		server, asToken, provSecret)
	must(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600))
	e := &env{t: t, dir: dir, c: c, there: there, bea: bea, remoteURL: remoteURL, bin: bin, config: filepath.Join(dir, "config.yaml"),
		prov: fmt.Sprintf("http://127.0.0.1:%d/_matrix/provision", brPort), provAuth: provSecret}
	e.startBridge()
	t.Cleanup(func() {
		e.stopBridge()
		data, _ := os.ReadFile(filepath.Join(dir, "bridge.log"))
		if keep := os.Getenv("E2E_LOG"); keep != "" {
			_ = os.WriteFile(keep, data, 0o600)
		}
		if t.Failed() {
			lines := strings.Split(string(data), "\n")
			t.Logf("bridge log (last 80 lines):\n%s", strings.Join(lines[max(0, len(lines)-80):], "\n"))
		}
	})
	return e
}

func (e *env) startBridge() {
	logFile, _ := os.OpenFile(filepath.Join(e.dir, "bridge.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	e.br = exec.Command(e.bin, "-c", e.config, "-n")
	e.br.Stdout, e.br.Stderr = logFile, logFile
	must(e.t, e.br.Start())
	if !until(30*time.Second, func() bool { return e.provision("GET", "/v3/whoami", nil, nil) == nil }) {
		e.t.Fatal("bridge did not start")
	}
}

func (e *env) stopBridge() {
	if e.br == nil {
		return
	}
	_ = e.br.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = e.br.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = e.br.Process.Kill()
		<-done
	}
	e.br = nil
}

type loginStep struct {
	LoginID string `json:"login_id"`
	Type    string `json:"type"`
	StepID  string `json:"step_id"`
	Input   struct {
		Fields []struct {
			ID string `json:"id"`
		} `json:"fields"`
	} `json:"user_input"`
}

// login signs the account there in with the password flow and returns the first step and the last.
func (e *env) login() (first, done *loginStep, err error) {
	first = &loginStep{}
	if err = e.provision("POST", "/v3/login/start/password", map[string]any{}, first); err != nil {
		return nil, nil, err
	}
	done = &loginStep{}
	err = e.provision("POST", "/v3/login/step/"+first.LoginID+"/"+first.StepID+"/user_input", map[string]string{
		"homeserver": e.remoteURL, "username": "alex", "password": "test-password-alex",
	}, done)
	return first, done, err
}

// state is the bridge's state of the login, or "" with none.
func (e *env) state() (loginID, state string) {
	var who struct {
		Logins []struct {
			ID    string `json:"id"`
			State struct {
				StateEvent string `json:"state_event"`
			} `json:"state"`
		} `json:"logins"`
	}
	if e.provision("GET", "/v3/whoami", nil, &who) != nil || len(who.Logins) == 0 {
		return "", ""
	}
	return who.Logins[0].ID, who.Logins[0].State.StateEvent
}

// friend is Bea on the other server: a real client with encryption (goolm), joining every invite and
// keeping what she receives, decrypted.
type friend struct {
	t      *testing.T
	client *mautrix.Client
	mu     sync.Mutex
	events []*event.Event
	ephem  []*event.Event
}

func newFriend(t *testing.T, dir, hsURL, name, regToken string) *friend {
	reg := &mx{t: t, base: hsURL}
	reg.register(name, regToken)
	ctx := context.Background()
	client, err := mautrix.NewClient(hsURL, "", "")
	must(t, err)
	resp, err := client.Login(ctx, &mautrix.ReqLogin{
		Type: mautrix.AuthTypePassword, Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: name},
		Password: "test-password-" + name, StoreCredentials: true, InitialDeviceDisplayName: "phone",
	})
	must(t, err)
	_ = resp
	db, err := dbutil.NewWithDialect("file:"+filepath.Join(dir, name+".db")+"?_txlock=immediate", "sqlite3-fk-wal")
	must(t, err)
	helper, err := cryptohelper.NewCryptoHelper(client, []byte("pickle-"+name), db)
	must(t, err)
	must(t, helper.Init(ctx))
	client.Crypto = helper
	f := &friend{t: t, client: client}
	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	keep := func(_ context.Context, evt *event.Event) {
		f.mu.Lock()
		f.events = append(f.events, evt)
		f.mu.Unlock()
	}
	for _, typ := range []event.Type{event.EventMessage, event.EventReaction, event.EventRedaction, event.EventSticker} {
		syncer.OnEventType(typ, keep)
	}
	ephem := func(_ context.Context, evt *event.Event) {
		f.mu.Lock()
		f.ephem = append(f.ephem, evt)
		f.mu.Unlock()
	}
	syncer.OnEventType(event.EphemeralEventReceipt, ephem)
	syncer.OnEventType(event.EphemeralEventTyping, ephem)
	syncer.OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
		if evt.GetStateKey() == string(client.UserID) && evt.Content.AsMember().Membership == event.MembershipInvite {
			_, _ = client.JoinRoomByID(ctx, evt.RoomID)
		}
	})
	syncCtx, cancel := context.WithCancel(ctx)
	go func() {
		for syncCtx.Err() == nil {
			_ = client.SyncWithContext(syncCtx)
			time.Sleep(200 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { cancel(); _ = helper.Close() })
	return f
}

// find is the first event Bea got for which match is true.
func (f *friend) find(match func(*event.Event) bool) *event.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, evt := range f.events {
		if match(evt) {
			return evt
		}
	}
	return nil
}

func (f *friend) all(match func(*event.Event) bool) []*event.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*event.Event
	for _, evt := range f.events {
		if match(evt) {
			out = append(out, evt)
		}
	}
	return out
}

func (f *friend) findEphemeral(match func(*event.Event) bool) *event.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, evt := range f.ephem {
		if match(evt) {
			return evt
		}
	}
	return nil
}

func body(text string) func(*event.Event) bool {
	return func(evt *event.Event) bool {
		return evt.Type == event.EventMessage && strings.Contains(evt.Content.AsMessage().Body, text)
	}
}

func (f *friend) send(room id.RoomID, typ event.Type, content any) id.EventID {
	resp, err := f.client.SendMessageEvent(context.Background(), room, typ, content)
	if err != nil {
		f.t.Error(err)
		return ""
	}
	return resp.EventID
}

// query reads one value from the bridge's database.
func (e *env) query(q string, args ...any) string {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(e.dir, "bridge.db")+"?mode=ro")
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	var v sql.NullString
	if err = db.QueryRow(q, args...).Scan(&v); err != nil {
		e.t.Logf("query %q: %v", q, err)
	}
	return v.String
}
