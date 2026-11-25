package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// Sign-out forgets the keys here at once, also when the other homeserver never answers the logout, and gives
// that logout up after logoutWait.
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
	done := make(chan struct{})
	go func() { mc.dropDevice(context.Background()); close(done) }()
	<-asked
	if _, err := os.Stat(keys); !os.IsNotExist(err) {
		t.Errorf("keys still here while the logout waits: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sign-out still waits for the homeserver after logoutWait")
	}
}
