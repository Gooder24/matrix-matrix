package connector

import (
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// UserLoginMetadata is one account on the other homeserver.
type UserLoginMetadata struct {
	Homeserver string      `json:"homeserver"`
	UserID     id.UserID   `json:"user_id"`
	DeviceID   id.DeviceID `json:"device_id"`
	Token      string      `json:"token"`
	// OwnDevice: the bridge made the device and ends it on sign-out; a token's own session is left alone.
	OwnDevice bool   `json:"own_device"`
	PickleKey string `json:"pickle_key"`
	// Since is when the account was signed in (ms); nothing older is bridged.
	Since int64 `json:"since"`
}

// ReactionMetadata keeps the reaction's event ID there, which is what a redaction names.
type ReactionMetadata struct {
	EventID id.EventID `json:"event_id"`
}

// A ghost's ID is the user's Matrix ID without the "@": @bea:example.org is ghost "bea:example.org",
// which the bridge's username template turns into @matrix_bea=3aexample.org:your.server.
func userID(mxid id.UserID) networkid.UserID {
	return networkid.UserID(strings.TrimPrefix(string(mxid), "@"))
}

func remoteMXID(uid networkid.UserID) id.UserID { return id.UserID("@" + string(uid)) }

func loginID(mxid id.UserID) networkid.UserLoginID { return networkid.UserLoginID(userID(mxid)) }

func portalKey(room id.RoomID, login *bridgev2.UserLogin) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(room), Receiver: login.ID}
}

func messageID(evt id.EventID) networkid.MessageID { return networkid.MessageID(evt) }
