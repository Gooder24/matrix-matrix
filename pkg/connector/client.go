package connector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// MatrixClient is one account on the other homeserver: a syncing client with its own device and keys.
type MatrixClient struct {
	Main      *MatrixConnector
	UserLogin *bridgev2.UserLogin

	mu     sync.Mutex
	client *mautrix.Client
	crypto *cryptohelper.CryptoHelper
	db     *dbutil.Database
	cancel context.CancelFunc
	done   chan struct{}
	ready  bool

	sent    sync.Map   // transaction IDs the bridge used there, so their echoes are dropped
	roomMu  sync.Mutex // rooms are made one at a time, also from the decryption callback
	bridged sync.Map   // room ID → true when the room there is itself a bridge (left out)
	typing  sync.Map   // room ID → map[id.UserID]bool of who types there
}

var (
	_ bridgev2.NetworkAPI                    = (*MatrixClient)(nil)
	_ bridgev2.EditHandlingNetworkAPI        = (*MatrixClient)(nil)
	_ bridgev2.ReactionHandlingNetworkAPI    = (*MatrixClient)(nil)
	_ bridgev2.RedactionHandlingNetworkAPI   = (*MatrixClient)(nil)
	_ bridgev2.ReadReceiptHandlingNetworkAPI = (*MatrixClient)(nil)
	_ bridgev2.TypingHandlingNetworkAPI      = (*MatrixClient)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI = (*MatrixClient)(nil)
)

func newClient(mc *MatrixConnector, login *bridgev2.UserLogin) *MatrixClient {
	return &MatrixClient{Main: mc, UserLogin: login}
}

func (mc *MatrixClient) meta() *UserLoginMetadata { return mc.UserLogin.Metadata.(*UserLoginMetadata) }

var reUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// keysPath is the login's SQLite file of keys and sync state, one per device.
func (mc *MatrixClient) keysPath() string {
	m := mc.meta()
	return filepath.Join(mc.Main.Config.CryptoDir, reUnsafe.ReplaceAllString(string(mc.UserLogin.ID), "_")+"."+
		reUnsafe.ReplaceAllString(string(m.DeviceID), "_")+".db")
}

// removeOtherKeys deletes the files of this login's earlier devices.
func (mc *MatrixClient) removeOtherKeys() {
	keep := mc.keysPath()
	prefix := filepath.Join(mc.Main.Config.CryptoDir, reUnsafe.ReplaceAllString(string(mc.UserLogin.ID), "_")+".")
	matches, _ := filepath.Glob(prefix + "*.db*")
	for _, f := range matches {
		if f != keep && f != keep+"-wal" && f != keep+"-shm" {
			_ = os.Remove(f)
		}
	}
}

func (mc *MatrixClient) removeKeys() {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(mc.keysPath() + suffix)
	}
}

// start opens the keys, turns encryption on and starts syncing. It returns once encryption works.
func (mc *MatrixClient) start(ctx context.Context) error {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.ready {
		return nil
	}
	m := mc.meta()
	log := mc.UserLogin.Log.With().Str("component", "remote").Logger()
	client, err := mautrix.NewClient(m.Homeserver, m.UserID, m.Token)
	if err != nil {
		return err
	}
	client.DeviceID = m.DeviceID
	client.Log = log
	db, err := dbutil.NewWithDialect(fmt.Sprintf("file:%s?_txlock=immediate", mc.keysPath()), "sqlite3-fk-wal")
	if err != nil {
		return err
	}
	_ = os.Chmod(mc.keysPath(), 0o600)
	helper, err := cryptohelper.NewCryptoHelper(client, []byte(m.PickleKey), db)
	if err != nil {
		_ = db.Close()
		return err
	}
	helper.DBAccountID = string(m.UserID)
	helper.DecryptErrorCallback = mc.cannotDecrypt
	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	syncer.FilterJSON = &mautrix.Filter{Room: &mautrix.RoomFilter{
		Timeline: &mautrix.FilterPart{Limit: 20},
		State:    &mautrix.FilterPart{LazyLoadMembers: true},
	}}
	if err = helper.Init(ctx); err != nil {
		_ = db.Close()
		return err
	}
	client.Crypto = helper
	for _, t := range []event.Type{event.EventMessage, event.EventSticker, event.EventReaction, event.EventRedaction} {
		syncer.OnEventType(t, mc.onEvent)
	}
	syncer.OnEventType(event.EphemeralEventReceipt, mc.onReceipt)
	syncer.OnEventType(event.EphemeralEventTyping, mc.onTyping)
	syncer.OnSync(mc.onSync)
	mc.client, mc.crypto, mc.db = client, helper, db
	syncCtx, cancel := context.WithCancel(log.WithContext(mc.Main.Bridge.BackgroundCtx))
	mc.cancel, mc.done, mc.ready = cancel, make(chan struct{}), true
	go mc.syncLoop(syncCtx, client, mc.done)
	return nil
}

func (mc *MatrixClient) syncLoop(ctx context.Context, client *mautrix.Client, done chan struct{}) {
	defer close(done)
	for ctx.Err() == nil {
		err := client.SyncWithContext(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, mautrix.MUnknownToken) {
			mc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials,
				Error: "matrix-signed-out", Message: "The other homeserver signed this device out. Sign in again."})
			return
		}
		mc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: "matrix-sync", Message: errorText(err)})
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

func (mc *MatrixClient) onSync(ctx context.Context, resp *mautrix.RespSync, since string) bool {
	mc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
	// After a gap (a restart, the server down) a room with more new events than one sync carries says so.
	// The very first sync of a device (no since) is limited by nature and has nothing to miss.
	if since != "" {
		for room, data := range resp.Rooms.Join {
			if data.Timeline.Limited {
				mc.notice(ctx, room, "Some messages of this chat did not come across while the bridge was away; another Matrix app shows them.")
			}
		}
	}
	return true
}

// Connect starts the account; when the homeserver is away it keeps trying, longer each time.
func (mc *MatrixClient) Connect(ctx context.Context) {
	mc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
	go func() {
		delay := 10 * time.Second
		for {
			err := mc.start(ctx)
			if err == nil {
				return
			}
			if errors.Is(err, mautrix.MUnknownToken) {
				mc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials,
					Error: "matrix-signed-out", Message: "The other homeserver signed this device out. Sign in again."})
				return
			}
			mc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: "matrix-start", Message: errorText(err)})
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay = min(delay*2, 10*time.Minute)
		}
	}()
}

func (mc *MatrixClient) Disconnect() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if !mc.ready {
		return
	}
	mc.ready = false
	mc.cancel()
	mc.client.StopSync()
	<-mc.done
	_ = mc.crypto.Close()
	_ = mc.db.Close()
}

func (mc *MatrixClient) IsLoggedIn() bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return mc.ready
}

// dropDevice ends the bridge's own device there and forgets its keys here.
func (mc *MatrixClient) dropDevice(ctx context.Context) {
	mc.Disconnect()
	m := mc.meta()
	if m.OwnDevice {
		if c, err := mautrix.NewClient(m.Homeserver, m.UserID, m.Token); err == nil {
			if _, err = c.Logout(ctx); err != nil {
				zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to end the device on the other homeserver")
			}
		}
	}
	mc.removeKeys()
}

func (mc *MatrixClient) LogoutRemote(ctx context.Context) { mc.dropDevice(ctx) }

func (mc *MatrixClient) IsThisUser(_ context.Context, uid networkid.UserID) bool {
	return remoteMXID(uid) == mc.meta().UserID
}

// remote is the running client, or an error the bridge shows when the account is not connected.
func (mc *MatrixClient) remote() (*mautrix.Client, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if !mc.ready {
		return nil, bridgev2.WrapErrorInStatus(errors.New("the Matrix account is not connected yet")).WithErrorAsMessage().WithSendNotice(true)
	}
	return mc.client, nil
}

func (mc *MatrixClient) sender(user id.UserID) bridgev2.EventSender {
	if user == mc.meta().UserID {
		return bridgev2.EventSender{IsFromMe: true, SenderLogin: mc.UserLogin.ID, Sender: userID(user)}
	}
	return bridgev2.EventSender{Sender: userID(user)}
}

// notice writes a note of the bridge into a room's portal, when there is one.
func (mc *MatrixClient) notice(ctx context.Context, room id.RoomID, text string) {
	portal, err := mc.Main.Bridge.GetExistingPortalByKey(ctx, portalKey(room, mc.UserLogin))
	if err != nil || portal == nil || portal.MXID == "" {
		return
	}
	_, err = mc.Main.Bridge.Bot.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{
		Parsed: &event.MessageEventContent{MsgType: event.MsgNotice, Body: text},
	}, nil)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to send a notice")
	}
}
