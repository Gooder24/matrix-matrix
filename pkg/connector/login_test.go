package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
)

// fakeHS answers what a sign-in asks of a homeserver. withKeys: the device already has encryption keys.
func fakeHS(t *testing.T, withKeys bool) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/versions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"versions":["v1.11"]}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/account/whoami", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"user_id":"@alex:remote.example","device_id":"KEEP"}`))
	})
	mux.HandleFunc("POST /_matrix/client/v1/login/get_token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errcode":"M_UNRECOGNIZED","error":"not here"}`))
	})
	mux.HandleFunc("POST /_matrix/client/v3/keys/query", func(w http.ResponseWriter, _ *http.Request) {
		if withKeys {
			_, _ = w.Write([]byte(`{"device_keys":{"@alex:remote.example":{"KEEP":{"user_id":"@alex:remote.example","device_id":"KEEP","algorithms":[],"keys":{}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"device_keys":{"@alex:remote.example":{}}}`))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// The address someone types may be a domain whose .well-known names the homeserver.
func TestWellKnown(t *testing.T) {
	hs := fakeHS(t, false)
	domain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/matrix/client" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"m.homeserver":{"base_url":"` + hs.URL + `/"}}`))
	}))
	defer domain.Close()
	ctx := context.Background()
	if got, err := resolveHomeserver(ctx, domain.URL); err != nil || got != hs.URL {
		t.Errorf("through .well-known: %q %v, want %q", got, err, hs.URL)
	}
	if got, err := resolveHomeserver(ctx, hs.URL+"/"); err != nil || got != hs.URL {
		t.Errorf("without .well-known: %q %v, want %q", got, err, hs.URL)
	}
	nothing := httptest.NewServer(http.NotFoundHandler())
	defer nothing.Close()
	if _, err := resolveHomeserver(ctx, nothing.URL); !hasCode(err, "INVALID_HOMESERVER") {
		t.Errorf("an address that is no homeserver: %v", err)
	}
}

// Without one-time login tokens there, a session may be used as it is only while it has no keys.
func TestTokenWithoutLoginTokens(t *testing.T) {
	ml := &MatrixLogin{Main: &MatrixConnector{}}
	for _, withKeys := range []bool{false, true} {
		hs := fakeHS(t, withKeys)
		client, err := mautrix.NewClient(hs.URL, "", "")
		if err != nil {
			t.Fatal(err)
		}
		meta := &UserLoginMetadata{OwnDevice: true}
		err = ml.tokenLogin(context.Background(), client, "given-token", meta)
		switch {
		case withKeys && !hasCode(err, "DEVICE_HAS_KEYS"):
			t.Errorf("a session with keys was taken: %v %+v", err, meta)
		case !withKeys && err != nil:
			t.Errorf("a session without keys was refused: %v", err)
		case !withKeys && (meta.DeviceID != "KEEP" || meta.Token != "given-token" || meta.OwnDevice):
			t.Errorf("a session without keys is not used as it is: %+v", meta)
		}
	}
}

func hasCode(err error, code string) bool {
	var re bridgev2.RespError
	return errors.As(err, &re) && re.ErrCode == "IO.GITHUB.GOODER24.MATRIX."+code
}
