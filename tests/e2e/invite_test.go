package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// An account on a homeserver of accept_invites_on takes invitations there (Weft Priamo), and with the server taken
// off the list it no longer does.
func TestInvites(t *testing.T) {
	e := setupWith(t, func(remoteURL string) string { return fmt.Sprintf("    accept_invites_on: [%q]", remoteURL+"/") })
	bea, ctx := e.bea, context.Background()
	alexThere := id.UserID("@alex:" + remote)
	e.there.noJoin.Store(true)
	if _, done, err := e.login(); err != nil || done.Type != "complete" {
		t.Fatalf("login: %v %+v", err, done)
	}
	if !until(wait, func() bool { _, s := e.state(); return s == "CONNECTED" }) {
		t.Fatal("not connected")
	}
	encrypted := []*event.Event{{Type: event.StateEncryption, Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}}}}
	invite := func() id.RoomID {
		room, err := bea.client.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "trusted_private_chat", IsDirect: true,
			Invite: []id.UserID{alexThere}, InitialState: encrypted})
		must(t, err)
		return room.RoomID
	}
	members := func(room id.RoomID) string {
		resp, err := bea.client.JoinedMembers(ctx, room)
		if err != nil {
			return ""
		}
		var names []string
		for user := range resp.Joined {
			names = append(names, string(user))
		}
		return strings.Join(names, " ")
	}

	first := invite()
	t.Run("an invitation is accepted", func(t *testing.T) {
		if !until(wait, func() bool { return strings.Contains(members(first), string(alexThere)) }) {
			t.Fatalf("the account never joined: %s", members(first))
		}
		bea.send(first, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "hello by invitation"})
		if !until(wait, func() bool { return e.c.roomWhere(isBody("hello by invitation")) != "" }) {
			t.Fatal("the invited room's message never came across")
		}
		room := e.c.roomWhere(isBody("hello by invitation"))
		var info map[string]any
		for _, evt := range e.c.roomEvents(room) {
			if evt["type"] == "m.bridge" {
				info = evt
			}
		}
		if got := str(info, "content", "channel", "fi.mau.receiver"); got != "alex:"+remote {
			t.Errorf("m.bridge names the login %q", got)
		}
	})

	t.Run("off the list, an invitation waits", func(t *testing.T) {
		e.stopBridge()
		data, err := os.ReadFile(e.config)
		must(t, err)
		lines := strings.Split(string(data), "\n")
		for i, l := range lines {
			if strings.Contains(l, "accept_invites_on") {
				lines[i] = "    accept_invites_on: []"
			}
		}
		must(t, os.WriteFile(e.config, []byte(strings.Join(lines, "\n")), 0o600))
		e.startBridge()
		if !until(wait, func() bool { _, s := e.state(); return s == "CONNECTED" }) {
			t.Fatal("not connected again")
		}
		second := invite()
		// A message after the invitation: once it is here, the sync that carried the invitation was handled.
		bea.send(first, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "after the second invitation"})
		if !until(wait, func() bool { return e.c.roomWhere(isBody("after the second invitation")) != "" }) {
			t.Fatal("the later message never came across")
		}
		if strings.Contains(members(second), string(alexThere)) {
			t.Error("the account joined a room although its server is not on the list")
		}
	})
}
