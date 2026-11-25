package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func failed(err error) error {
	return bridgev2.WrapErrorInStatus(err).WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true).
		WithStatus(event.MessageStatusFail).WithErrorReason(event.MessageStatusGenericError)
}

// encrypted asks the server when the state store does not know yet (right after sign-in): an unknown room is
// never treated as plain, and mautrix encrypts by the same store.
func (mc *MatrixClient) encrypted(ctx context.Context, client *mautrix.Client, room id.RoomID) (bool, error) {
	if known, err := client.StateStore.IsEncrypted(ctx, room); err != nil || known {
		return known, err
	}
	var content event.EncryptionEventContent
	err := client.StateEvent(ctx, room, event.StateEncryption, "", &content)
	if errors.Is(err, mautrix.MNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, client.StateStore.SetEncryptionEvent(ctx, room, &content)
}

// send puts an event there under a transaction ID of the bridge's own, so its echo in the sync is dropped.
func (mc *MatrixClient) send(ctx context.Context, client *mautrix.Client, room id.RoomID, evtType event.Type, content any) (*mautrix.RespSendEvent, error) {
	txn := "mxb" + randomHex(8)
	mc.sent.Store(txn, true)
	return client.SendMessageEvent(ctx, room, evtType, content, mautrix.ReqSendEvent{TransactionID: txn})
}

func (mc *MatrixClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	client, err := mc.remote()
	if err != nil {
		return nil, err
	}
	room := id.RoomID(msg.Portal.ID)
	isEncrypted, err := mc.encrypted(ctx, client, room)
	if err != nil {
		return nil, failed(fmt.Errorf("could not tell whether the room is encrypted: %w", err))
	}
	c := *msg.Content
	c.RelatesTo, c.NewContent = nil, nil
	mc.mentionsOut(ctx, &c)
	if c.URL != "" || c.File != nil {
		if err = mc.copyOut(ctx, client, &c, isEncrypted); err != nil {
			return nil, failed(fmt.Errorf("the file did not go across: %w", err))
		}
	}
	if msg.ThreadRoot != nil {
		fallback := id.EventID(msg.ThreadRoot.ID)
		if msg.ReplyTo != nil {
			fallback = id.EventID(msg.ReplyTo.ID)
		}
		c.RelatesTo = (&event.RelatesTo{}).SetThread(id.EventID(msg.ThreadRoot.ID), fallback)
		if msg.ReplyTo != nil {
			c.RelatesTo.IsFallingBack = false
		}
	} else if msg.ReplyTo != nil {
		c.RelatesTo = (&event.RelatesTo{}).SetReplyTo(id.EventID(msg.ReplyTo.ID))
	}
	resp, err := mc.send(ctx, client, room, msg.Event.Type, &c)
	if err != nil {
		return nil, failed(errors.New(errorText(err)))
	}
	return &bridgev2.MatrixMessageResponse{DB: &database.Message{
		ID: messageID(resp.EventID), SenderID: userID(mc.meta().UserID), Timestamp: time.Now(),
	}}, nil
}

// copyOut uploads a file there again: encrypted with fresh keys into an encrypted room.
func (mc *MatrixClient) copyOut(ctx context.Context, client *mautrix.Client, c *event.MessageEventContent, isEncrypted bool) error {
	limit := mc.Main.Config.MaxFileSize
	if c.Info != nil && int64(c.Info.Size) > limit {
		return fmt.Errorf("larger than %d MB", limit>>20)
	}
	src := c.URL
	if c.File != nil {
		src = c.File.URL
	}
	data, err := mc.Main.Bridge.Bot.DownloadMedia(ctx, src, c.File)
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("larger than %d MB", limit>>20)
	}
	mime, name := "application/octet-stream", c.FileName
	if name == "" {
		name = c.Body
	}
	if c.Info != nil {
		if c.Info.MimeType != "" {
			mime = c.Info.MimeType
		}
		c.Info.ThumbnailURL, c.Info.ThumbnailFile, c.Info.ThumbnailInfo = "", nil, nil
	}
	if isEncrypted {
		file := attachment.NewEncryptedFile()
		file.EncryptInPlace(data)
		up, err := client.UploadMedia(ctx, mautrix.ReqUploadMedia{ContentBytes: data, ContentType: "application/octet-stream"})
		if err != nil {
			return err
		}
		c.URL, c.File = "", &event.EncryptedFileInfo{EncryptedFile: *file, URL: up.ContentURI.CUString()}
		return nil
	}
	up, err := client.UploadMedia(ctx, mautrix.ReqUploadMedia{ContentBytes: data, ContentType: mime, FileName: name})
	if err != nil {
		return err
	}
	c.URL, c.File = up.ContentURI.CUString(), nil
	return nil
}

func (mc *MatrixClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	client, err := mc.remote()
	if err != nil {
		return err
	}
	newContent := *msg.Content
	newContent.RelatesTo, newContent.NewContent = nil, nil
	if newContent.URL != "" || newContent.File != nil {
		return failed(errors.New("only the text of a file's caption can be edited"))
	}
	mc.mentionsOut(ctx, &newContent)
	edit := &event.MessageEventContent{
		MsgType: newContent.MsgType, Body: "* " + newContent.Body, Mentions: newContent.Mentions, NewContent: &newContent,
	}
	if newContent.FormattedBody != "" {
		edit.Format, edit.FormattedBody = newContent.Format, "* "+newContent.FormattedBody
	}
	edit.RelatesTo = (&event.RelatesTo{}).SetReplace(id.EventID(msg.EditTarget.ID))
	if _, err = mc.send(ctx, client, id.RoomID(msg.Portal.ID), event.EventMessage, edit); err != nil {
		return failed(errors.New(errorText(err)))
	}
	return nil
}

func (mc *MatrixClient) PreHandleMatrixReaction(_ context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	key := msg.Content.RelatesTo.Key
	return bridgev2.MatrixReactionPreResponse{SenderID: userID(mc.meta().UserID), EmojiID: networkid.EmojiID(key), Emoji: key}, nil
}

func (mc *MatrixClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	client, err := mc.remote()
	if err != nil {
		return nil, err
	}
	content := &event.ReactionEventContent{RelatesTo: *(&event.RelatesTo{}).SetAnnotation(id.EventID(msg.TargetMessage.ID), msg.Content.RelatesTo.Key)}
	resp, err := mc.send(ctx, client, id.RoomID(msg.Portal.ID), event.EventReaction, content)
	if err != nil {
		return nil, failed(errors.New(errorText(err)))
	}
	return &database.Reaction{Metadata: &ReactionMetadata{EventID: resp.EventID}}, nil
}

func (mc *MatrixClient) redact(ctx context.Context, room id.RoomID, target id.EventID) error {
	client, err := mc.remote()
	if err != nil {
		return err
	}
	txn := "mxb" + randomHex(8)
	mc.sent.Store(txn, true)
	if _, err = client.RedactEvent(ctx, room, target, mautrix.ReqRedact{TxnID: txn}); err != nil {
		return failed(errors.New(errorText(err)))
	}
	return nil
}

func (mc *MatrixClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	meta, _ := msg.TargetReaction.Metadata.(*ReactionMetadata)
	if meta == nil || meta.EventID == "" {
		return failed(errors.New("the reaction's event there is not known"))
	}
	return mc.redact(ctx, id.RoomID(msg.Portal.ID), meta.EventID)
}

func (mc *MatrixClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	return mc.redact(ctx, id.RoomID(msg.Portal.ID), id.EventID(msg.TargetMessage.ID))
}

func (mc *MatrixClient) HandleMatrixReadReceipt(ctx context.Context, msg *bridgev2.MatrixReadReceipt) error {
	client, err := mc.remote()
	if err != nil {
		return nil // reading while it starts is not worth waiting for
	}
	target := msg.ExactMessage
	if target == nil {
		if target, err = mc.Main.Bridge.DB.Message.GetLastPartAtOrBeforeTime(ctx, msg.Portal.PortalKey, msg.ReadUpTo); err != nil || target == nil {
			return err
		}
	}
	return client.SetReadMarkers(ctx, id.RoomID(msg.Portal.ID), &mautrix.ReqSetReadMarkers{
		Read: id.EventID(target.ID), FullyRead: id.EventID(target.ID),
	})
}

func (mc *MatrixClient) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	client, err := mc.remote()
	if err != nil {
		return nil
	}
	_, err = client.UserTyping(ctx, id.RoomID(msg.Portal.ID), msg.IsTyping, 30*time.Second)
	return err
}
