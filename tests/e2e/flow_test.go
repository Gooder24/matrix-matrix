package e2e

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestBridge(t *testing.T) {
	e := setup(t)
	c, bea := e.c, e.bea
	ctx := context.Background()
	alexThere := id.UserID("@alex:" + remote)
	beaID := bea.client.UserID
	beaGhost := ghost(string(beaID))

	// Before sign-in: an encrypted room with a message the bridge's device will never have keys for.
	old, err := bea.client.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "trusted_private_chat", Invite: []id.UserID{alexThere},
		InitialState: []*event.Event{{Type: event.StateEncryption, Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}}}}})
	must(t, err)
	must(t, e.there.call("POST", "/_matrix/client/v3/join/"+url.PathEscape(string(old.RoomID)), map[string]any{}, nil))
	time.Sleep(time.Second)
	bea.send(old.RoomID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "an old message"})

	t.Run("sign-in", func(t *testing.T) {
		first, done, err := e.login()
		if err != nil {
			t.Fatal(err)
		}
		var fields []string
		for _, f := range first.Input.Fields {
			fields = append(fields, f.ID)
		}
		if strings.Join(fields, ",") != "homeserver,username,password" {
			t.Errorf("login fields %v", fields)
		}
		if done.Type != "complete" {
			t.Fatalf("login did not complete: %+v", done)
		}
		if !until(wait, func() bool { _, s := e.state(); return s == "CONNECTED" }) {
			id, s := e.state()
			t.Errorf("state %s %s", id, s)
		}
		var devices struct {
			Devices []struct {
				Name string `json:"display_name"`
			} `json:"devices"`
		}
		must(t, e.there.call("GET", "/_matrix/client/v3/devices", nil, &devices))
		found := false
		for _, d := range devices.Devices {
			found = found || d.Name == "Matrix bridge"
		}
		if !found {
			t.Errorf("no device of the bridge there: %+v", devices)
		}
	})

	// Ctrl+N: a new DM by @name:server, encrypted, the user in it at once.
	var dm string
	t.Run("new DM", func(t *testing.T) {
		var created struct {
			Room string `json:"dm_room_mxid"`
		}
		if err := e.provision("POST", "/v3/create_dm/"+url.PathEscape(string(beaID)), map[string]any{}, &created); err != nil || created.Room == "" {
			t.Fatalf("create DM: %v %+v", err, created)
		}
		dm = created.Room
		if !until(wait, func() bool { return c.joined(dm) }) {
			t.Fatal("the user never joined the new DM")
		}
		var bridgeState map[string]any
		if !until(wait, func() bool {
			for _, evt := range c.roomEvents(dm) {
				if evt["type"] == "m.bridge" {
					bridgeState = evt
				}
			}
			return bridgeState != nil
		}) || str(bridgeState, "content", "protocol", "id") != "github.com/Gooder24/matrix-matrix" {
			t.Errorf("no m.bridge state: %v", bridgeState)
		}
		var again struct {
			Room string `json:"dm_room_mxid"`
		}
		if err := e.provision("POST", "/v3/create_dm/"+url.PathEscape(string(beaID)), map[string]any{}, &again); err != nil || again.Room != dm {
			t.Errorf("a second create_dm made another room: %v %s %s", err, again.Room, dm)
		}
		if err := e.provision("POST", "/v3/create_dm/"+url.PathEscape("@nobody:"+remote), map[string]any{}, nil); err == nil {
			t.Error("a DM with someone who does not exist was made")
		}
	})
	if dm == "" {
		t.FailNow()
	}

	var thereRoom id.RoomID
	var sent string
	t.Run("a message out, encrypted", func(t *testing.T) {
		if !until(wait, func() bool {
			rooms, _ := bea.client.JoinedRooms(ctx)
			for _, r := range rooms.JoinedRooms {
				if r != old.RoomID {
					thereRoom = r
				}
			}
			return thereRoom != ""
		}) {
			t.Fatal("Bea never joined the DM there")
		}
		time.Sleep(500 * time.Millisecond)
		sent = c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": "hello Bea"})
		var got *event.Event
		until(wait, func() bool { got = bea.find(body("hello Bea")); return got != nil })
		if got == nil {
			t.Fatal("the message never arrived there")
		}
		if !got.Mautrix.WasEncrypted || got.Sender != alexThere {
			t.Errorf("arrived unencrypted or from someone else: encrypted=%v sender=%s", got.Mautrix.WasEncrypted, got.Sender)
		}
	})

	var answer map[string]any
	var answerThere id.EventID
	t.Run("an answer in, as a reply", func(t *testing.T) {
		hello := bea.find(body("hello Bea"))
		if hello == nil {
			t.Fatal("nothing to answer")
		}
		content := &event.MessageEventContent{MsgType: event.MsgText, Body: "hi Alex"}
		content.SetReply(hello)
		start := time.Now()
		answerThere = bea.send(thereRoom, event.EventMessage, content)
		until(wait, func() bool { answer = c.find(dm, isBody("hi Alex")); return answer != nil })
		if answer == nil {
			t.Fatal("the answer never came")
		}
		if took := time.Since(start); took > 10*time.Second {
			t.Errorf("took %s; something held it back", took)
		}
		if answer["sender"] != beaGhost {
			t.Errorf("sender %v, want %s", answer["sender"], beaGhost)
		}
		if str(answer, "content", "m.relates_to", "m.in_reply_to", "event_id") != sent {
			t.Errorf("not a reply to the user's message: %v", answer["content"])
		}
		if c.roomWhere(isBody("an old message")) != "" {
			t.Error("a message from before the sign-in came")
		}
		var profile struct {
			Name string `json:"displayname"`
		}
		_ = c.call("GET", "/_matrix/client/v3/profile/"+url.PathEscape(beaGhost), nil, &profile)
		if profile.Name != "bea" && profile.Name != string(beaID) {
			t.Errorf("ghost name %q", profile.Name)
		}
	})

	t.Run("a reply out", func(t *testing.T) {
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": "good to hear",
			"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": answer["event_id"]}}})
		var back *event.Event
		until(wait, func() bool { back = bea.find(body("good to hear")); return back != nil })
		if back == nil || back.Content.AsMessage().RelatesTo.GetReplyTo() != answerThere {
			t.Errorf("the reply did not answer Bea's message there: %v", back)
		}
	})

	t.Run("read receipts and typing", func(t *testing.T) {
		must(t, c.call("POST", "/_matrix/client/v3/rooms/"+url.PathEscape(dm)+"/receipt/m.read/"+url.PathEscape(str(answer, "event_id")), map[string]any{}, nil))
		if !until(wait, func() bool {
			return bea.findEphemeral(func(evt *event.Event) bool {
				if evt.Type != event.EphemeralEventReceipt {
					return false
				}
				_ = evt.Content.ParseRaw(evt.Type)
				_, ok := (*evt.Content.AsReceipt())[answerThere][event.ReceiptTypeRead][alexThere]
				return ok
			}) != nil
		}) {
			t.Error("reading here did not show there")
		}
		mine := bea.find(body("good to hear"))
		if mine != nil {
			must(t, bea.client.SendReceipt(ctx, thereRoom, mine.ID, event.ReceiptTypeRead, nil))
		}
		if !until(wait, func() bool {
			for _, evt := range c.roomEphemeral(dm) {
				if evt["type"] == "m.receipt" && strings.Contains(mustJSON(evt), beaGhost) {
					return true
				}
			}
			return false
		}) {
			t.Error("Bea's reading did not show here")
		}
		_, _ = bea.client.UserTyping(ctx, thereRoom, true, 20*time.Second)
		if !until(wait, func() bool {
			for _, evt := range c.roomEphemeral(dm) {
				if evt["type"] == "m.typing" && strings.Contains(mustJSON(evt), beaGhost) {
					return true
				}
			}
			return false
		}) {
			t.Error("Bea's typing did not show here")
		}
		_, _ = bea.client.UserTyping(ctx, thereRoom, false, 0)
	})

	t.Run("mentions", func(t *testing.T) {
		carol := "@carol:third.test"
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": "bea and carol, look",
			"format":         "org.matrix.custom.html",
			"formatted_body": `<a href="https://matrix.to/#/` + url.PathEscape(beaGhost) + `">bea</a> and <a href="https://matrix.to/#/` + carol + `">carol</a>, look`,
			"m.mentions":     map[string]any{"user_ids": []string{beaGhost, carol, c.userID}}})
		var got *event.Event
		until(wait, func() bool { got = bea.find(body("and carol, look")); return got != nil })
		if got == nil {
			t.Fatal("the mention never arrived there")
		}
		raw := string(got.Content.VeryRaw)
		users := got.Content.AsMessage().Mentions
		if users == nil || len(users.UserIDs) != 3 || users.UserIDs[0] != beaID || users.UserIDs[1] != id.UserID(carol) || users.UserIDs[2] != alexThere {
			t.Errorf("mentions there: %+v", users)
		}
		if strings.Contains(raw, "@matrix_") || strings.Contains(raw, "@alex:test") || !strings.Contains(raw, "matrix.to/#/@bea:remote.test") {
			t.Errorf("the mention names this server or not Bea there: %s", raw)
		}
		bea.send(thereRoom, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "alex: see this",
			Format: event.FormatHTML, FormattedBody: `<a href="https://matrix.to/#/@alex:remote.test">alex</a>: see this`,
			Mentions: &event.Mentions{UserIDs: []id.UserID{alexThere}}})
		var here map[string]any
		until(wait, func() bool { here = c.find(dm, isBody("see this")); return here != nil })
		if here == nil || !strings.Contains(mustJSON(here["content"]), `"user_ids":["@alex:test"]`) ||
			!strings.Contains(str(here, "content", "formatted_body"), "matrix.to/#/@alex:test") {
			t.Errorf("a mention of the user there does not name them here: %v", here)
		}
	})

	t.Run("edits", func(t *testing.T) {
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": "* hello Bea!",
			"m.new_content": map[string]any{"msgtype": "m.text", "body": "hello Bea!"},
			"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": sent}})
		var edit *event.Event
		until(wait, func() bool {
			edit = bea.find(func(evt *event.Event) bool {
				m := evt.Content.AsMessage()
				return evt.Type == event.EventMessage && m.RelatesTo != nil && m.RelatesTo.Type == event.RelReplace && m.NewContent != nil && m.NewContent.Body == "hello Bea!"
			})
			return edit != nil
		})
		if edit == nil || edit.Content.AsMessage().RelatesTo.EventID != bea.find(body("hello Bea")).ID {
			t.Errorf("the edit did not reach there as an edit: %v", edit)
		}
		bea.send(thereRoom, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "* hi Alex!",
			NewContent: &event.MessageEventContent{MsgType: event.MsgText, Body: "hi Alex!"},
			RelatesTo:  (&event.RelatesTo{}).SetReplace(answerThere)})
		if !until(wait, func() bool {
			return c.find(dm, func(e map[string]any) bool {
				return str(e, "content", "m.relates_to", "rel_type") == "m.replace" && str(e, "content", "m.relates_to", "event_id") == str(answer, "event_id") &&
					str(e, "content", "m.new_content", "body") == "hi Alex!"
			}) != nil
		}) {
			t.Error("Bea's edit did not come as an edit")
		}
	})

	t.Run("reactions", func(t *testing.T) {
		mine := c.send(dm, "m.reaction", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": answer["event_id"], "key": "👍"}})
		var there *event.Event
		until(wait, func() bool {
			there = bea.find(func(evt *event.Event) bool {
				return evt.Type == event.EventReaction && evt.Content.AsReaction().RelatesTo.EventID == answerThere && evt.Content.AsReaction().RelatesTo.Key == "👍"
			})
			return there != nil
		})
		if there == nil {
			t.Fatal("the reaction did not reach there")
		}
		if !there.Mautrix.WasEncrypted {
			t.Error("the reaction went into the encrypted room unencrypted")
		}
		helloThere := bea.find(body("hello Bea")).ID
		heart := bea.send(thereRoom, event.EventReaction, &event.ReactionEventContent{RelatesTo: event.RelatesTo{Type: event.RelAnnotation, EventID: helloThere, Key: "❤️"}})
		var heartHere map[string]any
		until(wait, func() bool {
			heartHere = c.find(dm, func(e map[string]any) bool {
				return e["type"] == "m.reaction" && str(e, "content", "m.relates_to", "event_id") == sent && e["sender"] == beaGhost
			})
			return heartHere != nil
		})
		if heartHere == nil {
			t.Fatal("Bea's reaction did not come")
		}
		_, err := bea.client.RedactEvent(ctx, thereRoom, heart)
		must(t, err)
		if !until(wait, func() bool {
			return c.find(dm, func(e map[string]any) bool {
				return e["type"] == "m.room.redaction" && redacts(e) == str(heartHere, "event_id")
			}) != nil
		}) {
			t.Error("taking Bea's reaction back did not reach here")
		}
		must(t, c.call("PUT", "/_matrix/client/v3/rooms/"+url.PathEscape(dm)+"/redact/"+url.PathEscape(mine)+"/r1", map[string]any{}, nil))
		if !until(wait, func() bool {
			return bea.find(func(evt *event.Event) bool { return evt.Type == event.EventRedaction && evt.Redacts == there.ID }) != nil
		}) {
			t.Error("taking the user's reaction back did not reach there")
		}
	})

	t.Run("files", func(t *testing.T) {
		data := []byte("\x89PNG\r\n\x1a\nnot really a picture, but bytes " + secret())
		file := attachment.NewEncryptedFile()
		enc := append([]byte(nil), data...)
		file.EncryptInPlace(enc)
		up, err := bea.client.UploadMedia(ctx, mautrix.ReqUploadMedia{ContentBytes: enc, ContentType: "application/octet-stream"})
		must(t, err)
		bea.send(thereRoom, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgImage, Body: "photo.png",
			Info: &event.FileInfo{MimeType: "image/png", Size: len(data)}, File: &event.EncryptedFileInfo{EncryptedFile: *file, URL: up.ContentURI.CUString()}})
		var img map[string]any
		until(wait, func() bool {
			img = c.find(dm, func(e map[string]any) bool { return str(e, "content", "msgtype") == "m.image" })
			return img != nil
		})
		if img == nil {
			t.Fatal("Bea's encrypted file did not come")
		}
		if got := c.download(str(img, "content", "url")); string(got) != string(data) {
			t.Errorf("the file here is not Bea's file (%d bytes)", len(got))
		}
		mineData := []byte("a document " + secret())
		mxc := c.upload(mineData, "doc.txt", "text/plain")
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.file", "body": "doc.txt", "url": mxc, "info": map[string]any{"mimetype": "text/plain", "size": len(mineData)}})
		var doc *event.Event
		until(wait, func() bool { doc = bea.find(body("doc.txt")); return doc != nil })
		if doc == nil {
			t.Fatal("the user's file did not reach there")
		}
		content := doc.Content.AsMessage()
		if content.File == nil || content.URL != "" {
			t.Fatalf("the file went there unencrypted: %+v", content)
		}
		uri, _ := content.File.URL.Parse()
		got, err := bea.client.DownloadBytes(ctx, uri)
		must(t, err)
		must(t, content.File.DecryptInPlace(got))
		if string(got) != string(mineData) {
			t.Error("the file there does not open to the user's file")
		}
		// A caption edited here is edited there, on the copy there.
		pic := c.upload(data, "pic.png", "image/png")
		picID := c.send(dm, "m.room.message", map[string]any{"msgtype": "m.image", "body": "first caption", "filename": "pic.png", "url": pic,
			"info": map[string]any{"mimetype": "image/png", "size": len(data)}})
		var picThere *event.Event
		until(wait, func() bool { picThere = bea.find(body("first caption")); return picThere != nil })
		if picThere == nil {
			t.Fatal("the captioned picture did not reach there")
		}
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.image", "body": "* second caption",
			"m.new_content": map[string]any{"msgtype": "m.image", "body": "second caption", "filename": "pic.png", "url": pic,
				"info": map[string]any{"mimetype": "image/png", "size": len(data)}},
			"m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": picID}})
		var captionEdit *event.Event
		until(wait, func() bool {
			captionEdit = bea.find(func(evt *event.Event) bool {
				m := evt.Content.AsMessage()
				return m.NewContent != nil && m.NewContent.Body == "second caption"
			})
			return captionEdit != nil
		})
		if captionEdit == nil {
			t.Fatal("the caption edit did not reach there")
		}
		if nc := captionEdit.Content.AsMessage().NewContent; nc.File == nil || nc.File.URL != picThere.Content.AsMessage().File.URL ||
			captionEdit.Content.AsMessage().RelatesTo.EventID != picThere.ID {
			t.Errorf("the caption edit does not keep the file there: %+v", nc)
		}
		big := make([]byte, 2<<20)
		bigURL := c.upload(big, "big.bin", "application/octet-stream")
		bigID := c.send(dm, "m.room.message", map[string]any{"msgtype": "m.file", "body": "big.bin", "url": bigURL, "info": map[string]any{"size": len(big)}})
		if !until(wait, func() bool {
			return c.find(dm, func(e map[string]any) bool {
				return e["type"] == "com.beeper.message_send_status" && str(e, "content", "m.relates_to", "event_id") == bigID && strings.HasPrefix(str(e, "content", "status"), "FAIL")
			}) != nil
		}) {
			t.Error("a file over the limit was not refused here")
		}
		time.Sleep(time.Second)
		if bea.find(body("big.bin")) != nil {
			t.Error("something of the refused file went there")
		}
	})

	t.Run("deletions", func(t *testing.T) {
		good := c.find(dm, isBody("good to hear"))
		goodThere := bea.find(body("good to hear"))
		must(t, c.call("PUT", "/_matrix/client/v3/rooms/"+url.PathEscape(dm)+"/redact/"+url.PathEscape(str(good, "event_id"))+"/r2", map[string]any{}, nil))
		if !until(wait, func() bool {
			return bea.find(func(evt *event.Event) bool { return evt.Type == event.EventRedaction && evt.Redacts == goodThere.ID }) != nil
		}) {
			t.Fatal("deleting here did not delete there")
		}
		time.Sleep(2 * time.Second)
		if n := len(bea.all(func(evt *event.Event) bool { return evt.Type == event.EventRedaction && evt.Redacts == goodThere.ID })); n != 1 {
			t.Errorf("deleted there %d times", n)
		}
		if n := countWhere(c.roomEvents(dm), func(e map[string]any) bool {
			return e["type"] == "m.room.redaction" && redacts(e) == str(good, "event_id")
		}); n != 1 {
			t.Errorf("deleted here %d times (an echo came back)", n)
		}
		_, err := bea.client.RedactEvent(ctx, thereRoom, answerThere)
		must(t, err)
		if !until(wait, func() bool {
			return c.find(dm, func(e map[string]any) bool {
				return e["type"] == "m.room.redaction" && redacts(e) == str(answer, "event_id")
			}) != nil
		}) {
			t.Error("Bea's deletion did not reach here")
		}
	})

	t.Run("order", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": fmt.Sprintf("out %d", i)})
		}
		var order []string
		until(wait, func() bool {
			order = nil
			for _, evt := range bea.all(body("out ")) {
				order = append(order, evt.Content.AsMessage().Body)
			}
			return len(order) == 5
		})
		if strings.Join(order, ",") != "out 0,out 1,out 2,out 3,out 4" {
			t.Errorf("order there %v", order)
		}
		for i := 0; i < 5; i++ {
			bea.send(thereRoom, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: fmt.Sprintf("in %d", i)})
		}
		until(wait, func() bool {
			order = nil
			for _, e := range c.roomEvents(dm) {
				if b := str(e, "content", "body"); e["type"] == "m.room.message" && strings.HasPrefix(b, "in ") {
					order = append(order, b)
				}
			}
			return len(order) == 5
		})
		if strings.Join(order, ",") != "in 0,in 1,in 2,in 3,in 4" {
			t.Errorf("order here %v", order)
		}
	})

	t.Run("join by alias", func(t *testing.T) {
		_, err := bea.client.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "public_chat", Name: "Klub", RoomAliasName: "klub"})
		must(t, err)
		var joined struct {
			RoomID string `json:"room_id"`
		}
		if err := e.provision("POST", "/v3/join", map[string]string{"room": "#klub:" + remote}, &joined); err != nil || joined.RoomID == "" {
			t.Fatalf("join: %v %+v", err, joined)
		}
		klub, _ := bea.client.ResolveAlias(ctx, id.RoomAlias("#klub:"+remote))
		time.Sleep(500 * time.Millisecond)
		bea.send(klub.RoomID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "welcome to the club"})
		if !until(wait, func() bool { return c.find(joined.RoomID, isBody("welcome to the club")) != nil }) {
			t.Error("a message in the joined room did not come")
		}
		var name struct {
			Name string `json:"name"`
		}
		_ = c.call("GET", "/_matrix/client/v3/rooms/"+url.PathEscape(joined.RoomID)+"/state/m.room.name/", nil, &name)
		if name.Name != "Klub" {
			t.Errorf("the joined room is called %q", name.Name)
		}
		if err := e.provision("POST", "/v3/join", map[string]string{"room": "#nothing:" + remote}, nil); err == nil {
			t.Error("an alias that does not exist was joined")
		}
		bridged, err := bea.client.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "public_chat", RoomAliasName: "bridged",
			InitialState: []*event.Event{{Type: event.StateBridge, StateKey: ptrTo("x"), Content: event.Content{Raw: map[string]any{"protocol": map[string]any{"id": "x"}}}}}})
		must(t, err)
		if err := e.provision("POST", "/v3/join", map[string]string{"room": "#bridged:" + remote}, nil); err == nil || !strings.Contains(err.Error(), "BRIDGED") {
			t.Errorf("a room that is a bridge was joined: %v", err)
		}
		time.Sleep(time.Second)
		if e.there.joined(string(bridged.RoomID)) {
			t.Error("the account stayed in the bridged room there")
		}
	})

	t.Run("a message it cannot read", func(t *testing.T) {
		// A Megolm message of a session the bridge's device never got the key for.
		_, err := bea.client.SendMessageEvent(ctx, thereRoom, event.EventEncrypted, map[string]any{
			"algorithm": "m.megolm.v1.aes-sha2", "ciphertext": "AwgAEhBnb3QgYm9ndXMgY2lwaGVydGV4dA", "session_id": "bm8gc3VjaCBzZXNzaW9u",
			"sender_key": "dGhpcyBpcyBub3QgYSByZWFsIGN1cnZlMjU1MTkga2V5", "device_id": "NOSUCHDEV",
		})
		must(t, err)
		if !until(time.Minute, func() bool {
			return c.find(dm, func(e map[string]any) bool {
				return e["sender"] == beaGhost && strings.Contains(str(e, "content", "body"), "cannot read (its key did not arrive)")
			}) != nil
		}) {
			t.Error("an undecryptable message left no trace here")
		}
	})

	t.Run("a gap while the bridge is away", func(t *testing.T) {
		e.stopBridge()
		for i := 0; i < 25; i++ {
			bea.send(thereRoom, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: fmt.Sprintf("while away %d", i)})
		}
		e.startBridge()
		if !until(wait, func() bool {
			return c.find(dm, func(e map[string]any) bool {
				return strings.Contains(str(e, "content", "body"), "did not come across while the bridge was away")
			}) != nil
		}) {
			t.Error("no notice of the gap")
		}
		if !until(wait, func() bool { return c.find(dm, isBody("while away 24")) != nil }) {
			t.Error("the newest message after the gap did not come")
		}
	})

	t.Run("signed out there", func(t *testing.T) {
		var devices struct {
			Devices []struct {
				ID   string `json:"device_id"`
				Name string `json:"display_name"`
			} `json:"devices"`
		}
		must(t, e.there.call("GET", "/_matrix/client/v3/devices", nil, &devices))
		var device string
		for _, d := range devices.Devices {
			if d.Name == "Matrix bridge" {
				device = d.ID
			}
		}
		if device == "" {
			t.Fatal("no device of the bridge there")
		}
		auth := map[string]any{"auth": map[string]any{"type": "m.login.password", "password": "test-password-alex",
			"identifier": map[string]any{"type": "m.id.user", "user": "alex"}}}
		must(t, e.there.call("DELETE", "/_matrix/client/v3/devices/"+url.PathEscape(device), auth, nil))
		if !until(time.Minute, func() bool { _, s := e.state(); return s == "BAD_CREDENTIALS" }) {
			_, s := e.state()
			t.Errorf("state %s after the device was signed out there", s)
		}
	})

	t.Run("signing in again keeps the chats", func(t *testing.T) {
		_, done, err := e.login()
		if err != nil || done.Type != "complete" {
			t.Fatalf("login again: %v %+v", err, done)
		}
		if !until(wait, func() bool { _, s := e.state(); return s == "CONNECTED" }) {
			t.Fatal("not connected after signing in again")
		}
		var devices struct {
			Devices []struct {
				Name string `json:"display_name"`
			} `json:"devices"`
		}
		must(t, e.there.call("GET", "/_matrix/client/v3/devices", nil, &devices))
		n := 0
		for _, d := range devices.Devices {
			if d.Name == "Matrix bridge" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d devices of the bridge there", n)
		}
		bea.send(thereRoom, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "are you back?"})
		if !until(wait, func() bool { return c.find(dm, isBody("are you back?")) != nil }) {
			t.Error("a message after signing in again did not come into the same room")
		}
		data := []byte("after the new sign-in " + secret())
		mxc := c.upload(data, "again.txt", "text/plain")
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.file", "body": "again.txt", "url": mxc, "info": map[string]any{"mimetype": "text/plain", "size": len(data)}})
		var doc *event.Event
		until(wait, func() bool { doc = bea.find(body("again.txt")); return doc != nil })
		if doc == nil {
			t.Fatal("a file after signing in again did not reach there")
		}
		if content := doc.Content.AsMessage(); content.File == nil || content.URL != "" {
			t.Errorf("after signing in again the file went unencrypted: %+v", content)
		}
	})

	t.Run("sign-out cleans up", func(t *testing.T) {
		loginID, _ := e.state()
		token := e.query("SELECT json_extract(metadata, '$.token') FROM user_login WHERE id = ?", loginID)
		if token == "" {
			t.Fatal("no token of the login in bridge.db")
		}
		must(t, e.provision("POST", "/v3/logout/"+url.PathEscape(loginID), map[string]any{}, nil))
		if !until(wait, func() bool { id, _ := e.state(); return id == "" }) {
			t.Error("the login is still there")
		}
		gone := &mx{t: t, base: e.remoteURL, token: token}
		if err := gone.call("GET", "/_matrix/client/v3/account/whoami", nil, nil); err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("the bridge's token still works there: %v", err)
		}
		if files, _ := filepath.Glob(filepath.Join(e.dir, "crypto", "*")); len(files) != 0 {
			t.Errorf("key files left: %v", files)
		}
		until(wait, func() bool { return e.query("SELECT count(*) FROM portal") == "0" })
		for _, table := range []string{"portal", "message", "user_login"} {
			if n := e.query("SELECT count(*) FROM " + table); n != "0" {
				t.Errorf("%s rows left in bridge.db: %s", table, n)
			}
		}
	})

	t.Run("sign-in refusals", func(t *testing.T) {
		try := func(flow string, input map[string]string) error {
			first := &loginStep{}
			if err := e.provision("POST", "/v3/login/start/"+flow, map[string]any{}, first); err != nil {
				return err
			}
			return e.provision("POST", "/v3/login/step/"+first.LoginID+"/"+first.StepID+"/user_input", input, nil)
		}
		if err := try("password", map[string]string{"homeserver": e.remoteURL, "username": "alex", "password": "wrong"}); err == nil || !strings.Contains(err.Error(), "LOGIN_FAILED") {
			t.Errorf("a wrong password: %v", err)
		}
		if err := try("password", map[string]string{"homeserver": "http://insecure.invalid", "username": "alex", "password": "x"}); err == nil || !strings.Contains(err.Error(), "INSECURE_HOMESERVER") {
			t.Errorf("a password over plain http: %v", err)
		}
		if id, _ := e.state(); id != "" {
			t.Error("a refused sign-in left a login")
		}
	})

	t.Run("token sign-in", func(t *testing.T) {
		first := &loginStep{}
		must(t, e.provision("POST", "/v3/login/start/token", map[string]any{}, first))
		done := &loginStep{}
		if err := e.provision("POST", "/v3/login/step/"+first.LoginID+"/"+first.StepID+"/user_input",
			map[string]string{"homeserver": e.remoteURL, "token": e.there.token}, done); err != nil || done.Type != "complete" {
			t.Fatalf("token sign-in: %v %+v", err, done)
		}
		if !until(wait, func() bool { _, s := e.state(); return s == "CONNECTED" }) {
			t.Error("not connected with a token")
		}
		bea.send(thereRoom, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "through the token"})
		if !until(wait, func() bool { return c.roomWhere(isBody("through the token")) != "" }) {
			t.Error("a message did not come with a token sign-in")
		}
		loginID, _ := e.state()
		must(t, e.provision("POST", "/v3/logout/"+url.PathEscape(loginID), map[string]any{}, nil))
		if err := e.there.call("GET", "/_matrix/client/v3/account/whoami", nil, nil); err != nil {
			t.Errorf("signing out of the bridge ended the given session: %v", err)
		}
	})
}
