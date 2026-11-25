# matrix-matrix

A puppeting bridge from your Matrix homeserver to your account on another Matrix homeserver (matrix.org,
beeper.com or any other), built on [mautrix-go](https://github.com/mautrix/go)'s bridgev2 framework. Your chats
there show up as rooms here, with the people in them as ghosts, so one client on your own server reaches both.

It is not part of the mautrix project. Issues go to this repository.

**Status:** early. Everything in [ROADMAP.md](ROADMAP.md) that is ticked is covered by an end-to-end test against
two throwaway [Tuwunel](https://github.com/matrix-construct/tuwunel) homeservers; it has not yet run against
matrix.org or beeper.com.

## Why not federation

Federation joins your server to the rooms themselves. That is often the better answer. This bridge is for when it
is not: your server keeps federation off, the account you want lives on a server you do not run (Beeper), or you
want the account there to stay the one that is in the rooms.

## How it works

* **Sign-in:** with homeserver, user name and password (the address can come from the user ID and
  `.well-known`; plain http is refused except on localhost), or with an access token for accounts without a
  password. Either way the bridge signs in a device of its own (a token is turned into a one-time login token
  through `login/get_token`; a token whose session has no keys yet is used as it is and left alone on sign-out).
* **Encryption:** the bridge's device has its own Olm/Megolm keys through mautrix-go's `goolm`, a pure-Go
  implementation, without libolm. Keys and sync state live in one SQLite file per login in `crypto_dir`.
  Signing out ends the device there and deletes the file. Never restore an old one from a backup: sign out and
  in again instead, or encryption with the other people's devices breaks.
* **Rooms:** every joined room there becomes a room here, created when something happens in it. A room without
  a name and with one other person is a DM. Invitations there are not accepted, and rooms that are themselves
  bridges there (`m.bridge`, `uk.half-shot.bridge`) are left out, or everything would arrive twice.
* **History:** only what comes after signing in. When the bridge was away long enough that a room has more new
  events than one sync carries, the room gets a notice that some messages did not come across.
* **Sending:** encrypted rooms get encrypted messages and files (with fresh keys), always. Mentions of ghosts turn
  into the people they stand for; other people of your own server are written as their name alone, so nothing
  there names your server. A message that cannot go (a file over `max_file_size`, say) fails with a status
  event, never silently.
* **New chats:** `create_dm` with `@name:server` opens the DM the account already has with that person, or makes
  a new encrypted one and remembers it in `m.direct`. `POST /_matrix/provision/v3/join` with
  `{"room": "#alias:server"}` joins a room there.

## Coming from Weft's Python bridge

This bridge replaces `server/matrix/bridge.py` of [Weft](https://github.com/Gooder24/weft), which does the same on
libolm (deprecated upstream). What a client sees differs in a few places:

* `m.bridge` `protocol.id` is `github.com/Gooder24/matrix-matrix` (bridgev2 takes it from the bridge type), not
  `matrix`; a client that picks icons or features by it needs the new ID.
* Ghosts follow the `username_template` of the config (`matrix_{{.}}` gives the same IDs as before).
* The login fields are in English and the homeserver has no default value.
* The provisioning API is bridgev2's whole API (`whoami`, `login`, `logout`, `create_dm`, `resolve_identifier`,
  `search_users`), plus `POST /v3/join`; it adds `com.beeper.room_features` and message status events.
* Logins of the Python bridge do not carry over: sign in again (an old device's keys must never come back).

## Setup

You need Go 1.26 or newer and a Matrix homeserver that supports application services.

```sh
go build -o matrix-matrix ./cmd/matrix-matrix
./matrix-matrix -e                 # writes config.yaml from the example
$EDITOR config.yaml                # homeserver, database, permissions; the network section is optional
./matrix-matrix -g                 # writes registration.yaml; add it to your homeserver
./matrix-matrix
```

Then send `login` to the bridge bot and follow its instructions, or use the bridgev2 provisioning API
(`/_matrix/provision/v3/login/start/password`). Turn on double puppeting so that your own messages from other apps
come from your Matrix account rather than from a ghost.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `device_name` | `Matrix bridge` | Name of the device the bridge adds to each account. |
| `crypto_dir` | `crypto` | Where each login's keys and sync state are kept. |
| `max_file_size` | `26214400` | Files larger than this do not cross, in either direction. |
| `members_at_once` | `20` | Rooms with up to this many people get all their members at once. |

## Measurements

On a 12-thread laptop, signed in to one account with one encrypted DM, syncing and idle for five minutes
(`E2E_MEASURE=5m TUWUNEL=… go test ./tests/e2e -run TestIdleUsage -v`): **29 MB** resident memory and
**0.5 s of CPU per hour**. The binary is 32 MB and links no C crypto library. For comparison, Weft's Python bridge
on libolm, running in production for 20 hours, used 44 MB and about 0.4 s of CPU per hour; the workloads differ
(real rooms there, a test account here), so take this as "about the same, a little less memory".

## Tests

```sh
go test ./...                                       # unit tests
TUWUNEL=/path/to/tuwunel go test ./tests/e2e -v     # two throwaway homeservers, a friend with encryption
```

The end-to-end test signs an account on one homeserver in through the bridge on the other and checks every
ticked item of the roadmap both ways, with a friend there who uses a real encrypting client. It does not cover
matrix.org's or Beeper's own quirks, rate limits, or a sync gap longer than one sync's worth of events.

## License

AGPL-3.0-or-later, like mautrix-go's bridges.
