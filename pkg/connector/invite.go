package connector

import (
	"context"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
)

// acceptsInvites: the login's homeserver is one of accept_invites_on, a server that itself lets only people the
// owner chose reach the account (Weft Priamo's allowlist), so every invitation there is one to take.
func (mc *MatrixClient) acceptsInvites() bool {
	home := strings.TrimRight(mc.meta().Homeserver, "/")
	for _, hs := range mc.Main.Config.AcceptInvitesOn {
		if strings.TrimRight(hs, "/") == home {
			return true
		}
	}
	return false
}

// onInvite joins a room the account was invited to there, on a homeserver of accept_invites_on; the room comes
// across with its first message, like every other.
func (mc *MatrixClient) onInvite(ctx context.Context, evt *event.Event) {
	if evt.Mautrix.EventSource&event.SourceInvite == 0 || evt.GetStateKey() != string(mc.meta().UserID) {
		return
	}
	if membership, _ := evt.Content.Raw["membership"].(string); membership != string(event.MembershipInvite) || !mc.acceptsInvites() {
		return
	}
	client, err := mc.remote()
	if err != nil {
		return
	}
	go func() {
		if _, err := client.JoinRoomByID(ctx, evt.RoomID); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Stringer("room_id", evt.RoomID).Msg("Failed to accept an invitation")
		}
	}()
}
