package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// Sign-out is done here at once, keys forgotten, also when the other homeserver never answers the logout: a
// sign-in right after it is never undone by it. The logout there is given up after logoutWait.
func TestSignOutWithoutAnAnswer(t *testing.T) {
	asked := make(chan struct{}, 1)
	release := make(chan struct{})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/logout" {
			asked <- struct{}{}
			<-release
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer hs.Close()
	defer close(release)
	mc := &MatrixClient{
		Main: &MatrixConnector{Config: Config{CryptoDir: t.TempDir()}},
		UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "alex", Metadata: &UserLoginMetadata{
			Homeserver: hs.URL, UserID: "@alex:remote.example", DeviceID: "WEFT", Token: "t", OwnDevice: true}}},
	}
	keys := mc.keysPath()
	if err := os.WriteFile(keys, []byte("keys"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func(was time.Duration) { logoutWait = was }(logoutWait)
	logoutWait = 2 * time.Second
	mc.dropDevice(context.Background())
	if _, err := os.Stat(keys); !os.IsNotExist(err) {
		t.Errorf("keys still here after sign-out: %v", err)
	}
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the other homeserver was never asked to end the device")
	}
	select {
	case <-mc.logoutDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the logout there was not given up after logoutWait")
	}
	if mc.start(mc.stoppable(context.Background())) == nil {
		t.Error("a start after sign-out went on")
	}
	first := mc.logoutDone
	mc.dropDevice(context.Background()) // a second sign-out, e.g. from a sign-in it cut short, does nothing more
	if mc.logoutDone != first {
		t.Error("a second sign-out asked the other homeserver again")
	}
}

// Only the bridge's own sign-out, which deletes the login, holds back a new sign-in of the account: a device a
// sign-in replaced (its NewLogin may still fail) never keeps the account refused.
func TestOnlyDeletionRefusesSignIn(t *testing.T) {
	client := func() *MatrixClient {
		return &MatrixClient{
			Main: &MatrixConnector{Config: Config{CryptoDir: t.TempDir()}},
			UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "alex", Metadata: &UserLoginMetadata{
				UserID: "@alex:remote.example", DeviceID: "WEFT"}}},
		}
	}
	replaced := client()
	replaced.dropDevice(context.Background())
	if replaced.beingDeleted() {
		t.Error("a device a sign-in replaced refuses the next sign-in")
	}
	signedOut := client()
	signedOut.LogoutRemote(context.Background())
	if !signedOut.beingDeleted() {
		t.Error("a sign-in during the bridge's sign-out is not refused")
	}
}

// A sign-in that waited for another sign-in of the account and whose caller left meanwhile ends the device it made
// there and leaves the account as the first one set it: it never drops the first one's device.
func TestLeftSignInEndsItsDevice(t *testing.T) {
	ended := make(chan string, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/logout" {
			ended <- r.Header.Get("Authorization")
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer hs.Close()
	ml := &MatrixLogin{Main: &MatrixConnector{}} // no bridge: reaching the logins at all fails the test
	meta := &UserLoginMetadata{Homeserver: hs.URL, UserID: "@alex:remote.example", DeviceID: "SECOND", Token: "second", OwnDevice: true}
	lock, _ := ml.Main.signIns.LoadOrStore(loginID(meta.UserID), &sync.Mutex{})
	lock.(*sync.Mutex).Lock() // the first sign-in of the account is still running
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := ml.complete(ctx, meta); result <- err }()
	cancel()
	lock.(*sync.Mutex).Unlock()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("a left sign-in: %v", err)
	}
	select {
	case auth := <-ended:
		if auth != "Bearer second" {
			t.Errorf("ended %q, not the left sign-in's device", auth)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the left sign-in's device was never ended there")
	}
}
