package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// wanted drops what was there before sign-in, the bridge's own echoes and rooms that are bridges there.
func (mc *MatrixClient) wanted(ctx context.Context, evt *event.Event) bool {
	if evt.Timestamp < mc.meta().Since {
		return false
	}
	if evt.Sender == mc.meta().UserID && evt.Unsigned.TransactionID != "" {
		if _, ours := mc.sent.LoadAndDelete(evt.Unsigned.TransactionID); ours {
			return false
		}
	}
	return !mc.isBridge(ctx, evt.RoomID)
}

// isBridge: a room that is a bridge on the other server (Beeper's own WhatsApp rooms, say) would bring everything twice.
func (mc *MatrixClient) isBridge(ctx context.Context, room id.RoomID) bool {
	if v, ok := mc.bridged.Load(room); ok {
		return v.(bool)
	}
	client, err := mc.remote()
	if err != nil {
		return false
	}
	state, err := client.State(ctx, room)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Stringer("room_id", room).Msg("Failed to read room state")
		return false
	}
	bridge := len(state[event.StateBridge]) > 0 || len(state[event.StateHalfShotBridge]) > 0
	mc.bridged.Store(room, bridge)
	return bridge
}

func (mc *MatrixClient) onEvent(ctx context.Context, evt *event.Event) {
	if !mc.wanted(ctx, evt) {
		return
	}
	meta := simplevent.EventMeta{
		PortalKey: portalKey(evt.RoomID, mc.UserLogin),
		Sender:    mc.sender(evt.Sender),
		Timestamp: time.UnixMilli(evt.Timestamp),
	}
	switch evt.Type {
	case event.EventMessage, event.EventSticker:
		content := evt.Content.AsMessage()
		if content.RelatesTo != nil && content.RelatesTo.Type == event.RelReplace && content.NewContent != nil {
			meta.Type = bridgev2.RemoteEventEdit
			mc.UserLogin.QueueRemoteEvent(&simplevent.Message[*event.Event]{
				EventMeta: meta, ID: messageID(evt.ID), TargetMessage: messageID(content.RelatesTo.EventID), Data: evt,
				ConvertEditFunc: mc.convertEdit,
			})
			return
		}
		meta.Type, meta.CreatePortal = bridgev2.RemoteEventMessage, true
		mc.ensureRoom(ctx, meta.PortalKey)
		mc.UserLogin.QueueRemoteEvent(&simplevent.Message[*event.Event]{
			EventMeta: meta, ID: messageID(evt.ID), Data: evt, ConvertMessageFunc: mc.convertMessage,
		})
	case event.EventReaction:
		rel := evt.Content.AsReaction().RelatesTo
		if rel.EventID == "" || rel.Key == "" {
			return
		}
		meta.Type = bridgev2.RemoteEventReaction
		mc.UserLogin.QueueRemoteEvent(&simplevent.Reaction{
			EventMeta: meta, TargetMessage: messageID(rel.EventID), EmojiID: networkid.EmojiID(rel.Key), Emoji: rel.Key,
			ReactionDBMeta: &ReactionMetadata{EventID: evt.ID},
		})
	case event.EventRedaction:
		target := evt.Redacts
		if target == "" {
			target = evt.Content.AsRedaction().Redacts
		}
		mc.onRedaction(ctx, meta, target)
	}
}

func (mc *MatrixClient) onRedaction(ctx context.Context, meta simplevent.EventMeta, target id.EventID) {
	if msg, err := mc.Main.Bridge.DB.Message.GetFirstPartByID(ctx, mc.UserLogin.ID, messageID(target)); err == nil && msg != nil {
		meta.Type = bridgev2.RemoteEventMessageRemove
		mc.UserLogin.QueueRemoteEvent(&simplevent.MessageRemove{EventMeta: meta, TargetMessage: messageID(target)})
		return
	}
	reaction, err := mc.reactionByEventID(ctx, target)
	if err != nil || reaction == nil {
		return
	}
	meta.Type = bridgev2.RemoteEventReactionRemove
	meta.Sender = mc.sender(remoteMXID(reaction.SenderID))
	mc.UserLogin.QueueRemoteEvent(&simplevent.Reaction{EventMeta: meta, TargetMessage: reaction.MessageID, EmojiID: reaction.EmojiID})
}

type reactionRow struct {
	MessageID networkid.MessageID
	SenderID  networkid.UserID
	EmojiID   networkid.EmojiID
}

// reactionByEventID finds a bridged reaction by its event ID there, which bridgev2 keeps only in its metadata.
func (mc *MatrixClient) reactionByEventID(ctx context.Context, evt id.EventID) (*reactionRow, error) {
	db := mc.Main.Bridge.DB.Database
	field := "json_extract(metadata, '$.event_id')"
	if db.Dialect == dbutil.Postgres {
		field = "metadata->>'event_id'"
	}
	var row reactionRow
	err := db.QueryRow(ctx, "SELECT message_id, sender_id, emoji_id FROM reaction WHERE bridge_id=$1 AND room_receiver=$2 AND "+field+"=$3",
		mc.Main.Bridge.ID, mc.UserLogin.ID, string(evt)).Scan(&row.MessageID, &row.SenderID, &row.EmojiID)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (mc *MatrixClient) onReceipt(ctx context.Context, evt *event.Event) {
	if mc.isBridge(ctx, evt.RoomID) {
		return
	}
	for target, kinds := range *evt.Content.AsReceipt() {
		for user, receipt := range kinds[event.ReceiptTypeRead] {
			mc.UserLogin.QueueRemoteEvent(&simplevent.Receipt{
				EventMeta: simplevent.EventMeta{
					Type: bridgev2.RemoteEventReadReceipt, PortalKey: portalKey(evt.RoomID, mc.UserLogin),
					Sender: mc.sender(user), Timestamp: receipt.Timestamp,
				},
				LastTarget: messageID(target),
			})
		}
	}
}

func (mc *MatrixClient) onTyping(ctx context.Context, evt *event.Event) {
	if mc.isBridge(ctx, evt.RoomID) {
		return
	}
	now := map[id.UserID]bool{}
	for _, user := range evt.Content.AsTyping().UserIDs {
		if user != mc.meta().UserID {
			now[user] = true
		}
	}
	before := map[id.UserID]bool{}
	if v, ok := mc.typing.Load(evt.RoomID); ok {
		before = v.(map[id.UserID]bool)
	}
	mc.typing.Store(evt.RoomID, now)
	send := func(user id.UserID, timeout time.Duration) {
		mc.UserLogin.QueueRemoteEvent(&simplevent.Typing{
			EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventTyping, PortalKey: portalKey(evt.RoomID, mc.UserLogin), Sender: mc.sender(user)},
			Timeout:   timeout,
		})
	}
	for user := range now {
		if !before[user] {
			send(user, 30*time.Second)
		}
	}
	for user := range before {
		if !now[user] {
			send(user, 0)
		}
	}
}

// cannotDecrypt: a message whose key never came still shows that it was there.
func (mc *MatrixClient) cannotDecrypt(evt *event.Event, err error) {
	ctx := mc.UserLogin.Log.WithContext(mc.Main.Bridge.BackgroundCtx)
	if !mc.wanted(ctx, evt) {
		return
	}
	zerolog.Ctx(ctx).Warn().Err(err).Stringer("event_id", evt.ID).Msg("Undecryptable message")
	mc.ensureRoom(ctx, portalKey(evt.RoomID, mc.UserLogin))
	mc.UserLogin.QueueRemoteEvent(&simplevent.Message[error]{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventMessage, PortalKey: portalKey(evt.RoomID, mc.UserLogin), CreatePortal: true,
			Sender: mc.sender(evt.Sender), Timestamp: time.UnixMilli(evt.Timestamp),
		},
		ID: messageID(evt.ID), Data: err,
		ConvertMessageFunc: func(context.Context, *bridgev2.Portal, bridgev2.MatrixAPI, error) (*bridgev2.ConvertedMessage, error) {
			return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{Type: event.EventMessage, Content: &event.MessageEventContent{
				MsgType: event.MsgNotice, Body: "An encrypted message the bridge cannot read (its key did not arrive).",
			}}}}, nil
		},
	})
}

func (mc *MatrixClient) convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, evt *event.Event) (*bridgev2.ConvertedMessage, error) {
	content := evt.Content.AsMessage()
	part := mc.incomingPart(ctx, portal, intent, evt.Type, content)
	out := &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{part}}
	if rel := content.RelatesTo; rel != nil {
		if rel.Type == event.RelThread && rel.EventID != "" {
			root := messageID(rel.EventID)
			out.ThreadRoot = &root
		}
		if reply := rel.GetReplyTo(); reply != "" && !rel.IsFallingBack {
			out.ReplyTo = &networkid.MessageOptionalPartID{MessageID: messageID(reply)}
		}
	}
	return out, nil
}

func (mc *MatrixClient) convertEdit(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, evt *event.Event) (*bridgev2.ConvertedEdit, error) {
	content := evt.Content.AsMessage().NewContent
	part := mc.incomingPart(ctx, portal, intent, event.EventMessage, content)
	return &bridgev2.ConvertedEdit{ModifiedParts: []*bridgev2.ConvertedEditPart{part.ToEditPart(existing[0])}}, nil
}

// incomingPart copies a message from there: its people named as they are here, its file uploaded here again.
func (mc *MatrixClient) incomingPart(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, evtType event.Type, content *event.MessageEventContent) *bridgev2.ConvertedMessagePart {
	c := *content
	c.RelatesTo, c.NewContent = nil, nil
	if c.Mentions != nil {
		mentions := *c.Mentions
		c.Mentions = &mentions
	}
	mc.mentionsIn(ctx, &c)
	if c.URL != "" || c.File != nil {
		if err := mc.copyIn(ctx, portal, intent, &c); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("File did not come across")
			c.URL, c.File, c.Info = "", nil, nil
			name := c.Body
			if name == "" {
				name = "File"
			}
			c.MsgType, c.Body, c.Format, c.FormattedBody = event.MsgNotice, name+" (the file did not come across)", "", ""
			evtType = event.EventMessage
		}
	}
	return &bridgev2.ConvertedMessagePart{Type: evtType, Content: &c}
}

func (mc *MatrixClient) copyIn(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, c *event.MessageEventContent) error {
	limit := mc.Main.Config.MaxFileSize
	if c.Info != nil && int64(c.Info.Size) > limit {
		return fmt.Errorf("larger than %d bytes", limit)
	}
	client, err := mc.remote()
	if err != nil {
		return err
	}
	src := c.URL
	if c.File != nil {
		src = c.File.URL
	}
	uri, err := src.Parse()
	if err != nil {
		return err
	}
	data, err := client.DownloadBytes(ctx, uri)
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("larger than %d bytes", limit)
	}
	if c.File != nil {
		if err = c.File.DecryptInPlace(data); err != nil {
			return err
		}
	}
	name, mime := c.FileName, ""
	if name == "" {
		name = c.Body
	}
	if c.Info != nil {
		mime = c.Info.MimeType
		c.Info.ThumbnailURL, c.Info.ThumbnailFile, c.Info.ThumbnailInfo = "", nil, nil
	}
	url, file, err := intent.UploadMedia(ctx, portal.MXID, data, name, mime)
	if err != nil {
		return err
	}
	c.URL, c.File = url, file
	return nil
}

// ensureRoom makes a portal's room here before its first message, one after another from the sync.
// Left to bridgev2, rooms of several portals are made at once, and Tuwunel gives two rooms the bot
// creates in the same millisecond the same ID (room version 12 names a room by its create event).
func (mc *MatrixClient) ensureRoom(ctx context.Context, key networkid.PortalKey) {
	mc.roomMu.Lock()
	defer mc.roomMu.Unlock()
	portal, err := mc.Main.Bridge.GetPortalByKey(ctx, key)
	if err != nil || portal.MXID != "" || !mc.Main.Bridge.Config.PortalCreateFilter.ShouldAllow(mc.UserLogin.ID, key) {
		return
	}
	info, err := mc.GetChatInfo(ctx, portal)
	if err == nil {
		err = portal.CreateMatrixRoom(ctx, mc.UserLogin, info)
	}
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("portal", string(key.ID)).Msg("Failed to make a room; bridgev2 tries again with the message")
	}
}
