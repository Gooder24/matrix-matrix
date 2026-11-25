package connector

import (
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// toy maps two people there to their ghosts here and leaves everyone else out.
func toy(u id.UserID) id.UserID {
	switch u {
	case "@bea:remote.example":
		return "@matrix_bea=3aremote.example:home.example"
	case "@alex:remote.example":
		return "@alex:home.example"
	}
	return ""
}

func TestRewriteLinks(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		toLocal        bool
	}{
		{"a link", `hi <a href="https://matrix.to/#/@bea:remote.example">Bea</a>!`,
			`hi <a href="https://matrix.to/#/@matrix_bea%3D3aremote.example:home.example">Bea</a>!`, true},
		{"encoded @", `<a href='https://matrix.to/#/%40alex:remote.example?via=x'>Alex</a>`,
			`<a href="https://matrix.to/#/@alex:home.example">Alex</a>`, true},
		{"no one", `<a href="https://matrix.to/#/@carol:third.example">Carol</a> and`, `Carol and`, false},
		{"bare going out", `see https://matrix.to/#/@carol:third.example.`, `see .`, false},
		{"bare coming in", `see https://matrix.to/#/@carol:third.example.`, `see https://matrix.to/#/@carol:third.example.`, true},
		{"bare mapped", `(https://matrix.to/#/@bea:remote.example?via=a&amp;via=b)`,
			`(https://matrix.to/#/@matrix_bea%3D3aremote.example:home.example)`, true},
		{"an auto-linked text", `<a href="https://matrix.to/#/@bea:remote.example">https://matrix.to/#/@bea:remote.example</a>`,
			`<a href="https://matrix.to/#/@matrix_bea%3D3aremote.example:home.example">https://matrix.to/#/@matrix_bea%3D3aremote.example:home.example</a>`, true},
		{"room links stay", `<a href="https://matrix.to/#/#club:remote.example">club</a>`, `<a href="https://matrix.to/#/#club:remote.example">club</a>`, false},
	} {
		if got := rewriteLinks(tc.in, toy, tc.toLocal); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestTranslateMentions(t *testing.T) {
	c := &event.MessageEventContent{Body: "Bea, Carol", Format: event.FormatHTML,
		FormattedBody: `<a href="https://matrix.to/#/@bea:remote.example">Bea</a>, <a href="https://matrix.to/#/@carol:third.example">Carol</a>`,
		Mentions:      &event.Mentions{UserIDs: []id.UserID{"@bea:remote.example", "@carol:third.example"}, Room: true}}
	translateMentions(c, toy, false)
	if len(c.Mentions.UserIDs) != 1 || c.Mentions.UserIDs[0] != "@matrix_bea=3aremote.example:home.example" || !c.Mentions.Room {
		t.Errorf("mentions %+v", c.Mentions)
	}
	if c.FormattedBody != `<a href="https://matrix.to/#/@matrix_bea%3D3aremote.example:home.example">Bea</a>, Carol` {
		t.Errorf("body %s", c.FormattedBody)
	}
	plain := &event.MessageEventContent{Body: "no html"}
	translateMentions(plain, toy, true)
	if plain.Mentions != nil || plain.FormattedBody != "" {
		t.Errorf("a plain message changed: %+v", plain)
	}
}

func TestIDs(t *testing.T) {
	u := id.UserID("@bea:remote.example")
	if userID(u) != "bea:remote.example" || remoteMXID(userID(u)) != u || loginID(u) != "bea:remote.example" {
		t.Error("a ghost ID does not go back to the same Matrix ID")
	}
}
