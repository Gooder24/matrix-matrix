package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

func (mc *MatrixConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		Provisioning: bridgev2.ProvisioningCapabilities{
			ResolveIdentifier: bridgev2.ResolveIdentifierCapabilities{CreateDM: true, LookupUsername: true},
		},
	}
}

func fileCaps(maxSize int64) *event.FileFeatures {
	return &event.FileFeatures{
		MimeTypes: map[string]event.CapabilitySupportLevel{"*/*": event.CapLevelFullySupported},
		Caption:   event.CapLevelFullySupported,
		MaxSize:   maxSize,
	}
}

// GetCapabilities: the other side is Matrix too, so everything the bridge carries is fully supported.
// Editing a file is not carried (the copy there has another URL), so edits are text only.
func (mc *MatrixClient) GetCapabilities(_ context.Context, _ *bridgev2.Portal) *event.RoomFeatures {
	size := mc.Main.Config.MaxFileSize
	formatting := event.FormattingFeatureMap{}
	for _, f := range []event.FormattingFeature{
		event.FmtBold, event.FmtItalic, event.FmtUnderline, event.FmtStrikethrough, event.FmtInlineCode,
		event.FmtCodeBlock, event.FmtBlockquote, event.FmtInlineLink, event.FmtUserLink, event.FmtRoomLink,
		event.FmtUnorderedList, event.FmtOrderedList, event.FmtHeaders, event.FmtSpoiler,
	} {
		formatting[f] = event.CapLevelFullySupported
	}
	return &event.RoomFeatures{
		ID:         "io.github.gooder24.matrix.capabilities.2026_11_25",
		Formatting: formatting,
		File: event.FileFeatureMap{
			event.MsgImage:      fileCaps(size),
			event.MsgVideo:      fileCaps(size),
			event.MsgAudio:      fileCaps(size),
			event.MsgFile:       fileCaps(size),
			event.CapMsgSticker: fileCaps(size),
		},
		Thread:              event.CapLevelFullySupported,
		Reply:               event.CapLevelFullySupported,
		Edit:                event.CapLevelFullySupported,
		Delete:              event.CapLevelFullySupported,
		Reaction:            event.CapLevelFullySupported,
		ReadReceipts:        true,
		TypingNotifications: true,
	}
}
