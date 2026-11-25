package connector

import (
	"context"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// avatar is a picture on the other homeserver, downloaded only when it changed.
func (mc *MatrixClient) avatar(uri id.ContentURIString) *bridgev2.Avatar {
	if uri == "" {
		return &bridgev2.Avatar{Remove: true}
	}
	parsed, err := uri.Parse()
	if err != nil {
		return nil
	}
	return &bridgev2.Avatar{
		ID: networkid.AvatarID(uri),
		Get: func(ctx context.Context) ([]byte, error) {
			client, err := mc.remote()
			if err != nil {
				return nil, err
			}
			return client.DownloadBytes(ctx, parsed)
		},
	}
}

// GetChatInfo reads the room's state there: name, topic, picture and the people in it. A room without
// a name and with one other person is a DM.
func (mc *MatrixClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	client, err := mc.remote()
	if err != nil {
		return nil, err
	}
	state, err := client.State(ctx, id.RoomID(portal.ID))
	if err != nil {
		return nil, err
	}
	me := mc.meta().UserID
	members := &bridgev2.ChatMemberList{MemberMap: bridgev2.ChatMemberMap{}}
	members.MemberMap.Set(bridgev2.ChatMember{EventSender: mc.sender(me), Membership: event.MembershipJoin})
	var others []*event.MemberEventContent
	var otherIDs []id.UserID
	for key, evt := range state[event.StateMember] {
		_ = evt.Content.ParseRaw(evt.Type)
		content := evt.Content.AsMember()
		if id.UserID(key) == me || (content.Membership != event.MembershipJoin && content.Membership != event.MembershipInvite) {
			continue
		}
		others, otherIDs = append(others, content), append(otherIDs, id.UserID(key))
	}
	members.TotalMemberCount = len(others) + 1
	// A big public room's people come in when they write; a small one's all at once.
	members.IsFull = len(others) <= mc.Main.Config.MembersAtOnce
	for i, content := range others {
		if i >= mc.Main.Config.MembersAtOnce {
			break
		}
		info := &bridgev2.UserInfo{Avatar: mc.avatar(content.AvatarURL)}
		if content.Displayname != "" {
			info.Name = ptr.Ptr(content.Displayname)
		}
		members.MemberMap.Set(bridgev2.ChatMember{
			EventSender: mc.sender(otherIDs[i]), Membership: content.Membership, UserInfo: info,
		})
	}
	info := &bridgev2.ChatInfo{Members: members}
	name := stateString(state, event.StateRoomName, func(c *event.Content) string { return c.AsRoomName().Name })
	if len(others) == 1 && name == "" {
		info.Type = ptr.Ptr(database.RoomTypeDM)
		members.OtherUserID = userID(otherIDs[0])
		return info, nil
	}
	if name == "" {
		var names []string
		for i, content := range others {
			if i == 3 {
				break
			}
			names = append(names, cmpOr(content.Displayname, string(otherIDs[i])))
		}
		name = strings.Join(names, ", ")
	}
	info.Type = ptr.Ptr(database.RoomTypeDefault)
	info.Name = ptr.Ptr(name)
	info.Topic = ptr.Ptr(stateString(state, event.StateTopic, func(c *event.Content) string { return c.AsTopic().Topic }))
	if evt := stateEvent(state, event.StateRoomAvatar); evt != nil {
		info.Avatar = mc.avatar(evt.Content.AsRoomAvatar().URL)
	} else {
		info.Avatar = &bridgev2.Avatar{Remove: true}
	}
	return info, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func stateEvent(state mautrix.RoomStateMap, t event.Type) *event.Event {
	evt := state[t][""]
	if evt == nil {
		return nil
	}
	_ = evt.Content.ParseRaw(evt.Type)
	return evt
}

func stateString(state mautrix.RoomStateMap, t event.Type, get func(*event.Content) string) string {
	if evt := stateEvent(state, t); evt != nil {
		return get(&evt.Content)
	}
	return ""
}

// GetUserInfo is the person's profile there. Many servers show profiles only to people who share a
// room, so a refusal leaves the ghost as it is.
func (mc *MatrixClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	client, err := mc.remote()
	if err != nil {
		return nil, err
	}
	profile, err := client.GetProfile(ctx, remoteMXID(ghost.ID))
	if err != nil {
		return nil, nil
	}
	info := &bridgev2.UserInfo{Name: ptr.Ptr(cmpOr(profile.DisplayName, string(remoteMXID(ghost.ID)))),
		Identifiers: []string{"matrix:" + string(remoteMXID(ghost.ID))}}
	info.Avatar = mc.avatar(profile.AvatarURL.CUString())
	return info, nil
}
