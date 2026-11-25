package connector

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// A user link, written with "@" or "%40" (Element and Beeper encode it), alone or as the whole <a> around it.
var reMatrixTo = regexp.MustCompile(`(?is)<a\s(?:[^>]*?\s)?href=["']https?://matrix\.to/#/((?:@|%40)[^"'<>\s?/&]+)[^"'<>\s]*["'][^>]*>(.*?)</a>` +
	`|https?://matrix\.to/#/((?:@|%40)[^"'<>\s?/&]+)(?:\?[^"'<>\s&]*(?:&amp;[^"'<>\s&]*)*)?`)

var reTrailing = regexp.MustCompile(`[.,;:!?)\]]+$`) // sentence punctuation after a bare link is not part of it

func matrixToHref(user id.UserID) string {
	var b strings.Builder
	for _, c := range []byte(user) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.~@:/", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(string("0123456789abcdef"[c>>4])+string("0123456789abcdef"[c&15])))
		}
	}
	return "https://matrix.to/#/" + b.String()
}

// rewriteLinks maps every user link of an HTML body; a link to no one (mapped to "") keeps its text alone, and
// a bare one going out disappears.
func rewriteLinks(html string, other func(id.UserID) id.UserID, toLocal bool) string {
	var out strings.Builder
	last := 0
	for _, m := range reMatrixTo.FindAllStringSubmatchIndex(html, -1) {
		out.WriteString(html[last:m[0]])
		last = m[1]
		if m[2] >= 0 {
			uid, _ := url.PathUnescape(html[m[2]:m[3]])
			text := rewriteLinks(html[m[4]:m[5]], other, toLocal) // an auto-linked text is a link too
			if mapped := other(id.UserID(uid)); mapped != "" {
				out.WriteString(`<a href="` + matrixToHref(mapped) + `">` + text + `</a>`)
			} else {
				out.WriteString(text)
			}
			continue
		}
		raw := html[m[0]:m[1]]
		tail := reTrailing.FindString(raw)
		raw = raw[:len(raw)-len(tail)]
		uid := ""
		if sub := reMatrixTo.FindStringSubmatch(raw); sub != nil {
			uid, _ = url.PathUnescape(sub[3])
		}
		switch mapped := other(id.UserID(uid)); {
		case mapped != "":
			out.WriteString(matrixToHref(mapped))
		case toLocal:
			out.WriteString(raw)
		}
		out.WriteString(tail)
	}
	out.WriteString(html[last:])
	return out.String()
}

func translateMentions(c *event.MessageEventContent, other func(id.UserID) id.UserID, toLocal bool) {
	if c.Mentions != nil {
		var users []id.UserID
		for _, u := range c.Mentions.UserIDs {
			if mapped := other(u); mapped != "" {
				users = append(users, mapped)
			}
		}
		c.Mentions = &event.Mentions{UserIDs: users, Room: c.Mentions.Room}
	}
	if c.Format == event.FormatHTML && c.FormattedBody != "" {
		c.FormattedBody = rewriteLinks(c.FormattedBody, other, toLocal)
	}
}

// mentionsIn: the account's own ID there becomes its owner here, anyone else their ghost.
func (mc *MatrixClient) mentionsIn(_ context.Context, c *event.MessageEventContent) {
	me := mc.meta().UserID
	translateMentions(c, func(u id.UserID) id.UserID {
		if !strings.HasPrefix(string(u), "@") || !strings.Contains(string(u), ":") {
			return ""
		}
		if u == me {
			return mc.UserLogin.UserMXID
		}
		return mc.Main.Bridge.Matrix.FormatGhostMXID(userID(u))
	}, true)
}

// mentionsOut: a ghost becomes the person it stands for, the owner their account there; other people of this
// server have no account there and go out as their name alone, so nothing names this server.
func (mc *MatrixClient) mentionsOut(_ context.Context, c *event.MessageEventContent) {
	me := mc.meta().UserID
	local := ":" + strings.ToLower(mc.Main.Bridge.Matrix.ServerName())
	translateMentions(c, func(u id.UserID) id.UserID {
		if u == mc.UserLogin.UserMXID {
			return me
		}
		if ghost, ok := mc.Main.Bridge.Matrix.ParseGhostMXID(u); ok {
			return remoteMXID(ghost)
		}
		if !strings.HasPrefix(string(u), "@") || strings.HasSuffix(strings.ToLower(string(u)), local) {
			return ""
		}
		return u
	}, false)
}
