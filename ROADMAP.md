# Features & roadmap
* Matrix (here) → Matrix (there)
  * [x] Message content
    * [x] Plain text
    * [x] Formatted messages
    * [x] Mentions (ghosts become the people they stand for; nobody of this server is named there)
    * [x] Media/files and stickers, encrypted again in encrypted rooms
    * [x] Replies and threads
  * [x] Message edits (text)
  * [x] Message redactions
  * [x] Reactions and their removal
  * [x] Typing notifications
  * [x] Read receipts
  * [ ] Room metadata changes
  * [ ] Membership actions (invite, kick, leave)
* Matrix (there) → Matrix (here)
  * [x] Message content, encrypted or not
    * [x] Plain text
    * [x] Formatted messages
    * [x] Mentions
    * [x] Media/files and stickers (decrypted and uploaded again)
    * [x] Replies and threads
  * [x] Message edits
  * [x] Message deletions
  * [x] Reactions and their removal
  * [x] Typing notifications
  * [x] Read receipts
  * [x] Your own messages from other apps, as you (with double puppeting)
  * [x] A notice for a message whose key never arrived
  * [x] A notice when messages were missed while the bridge was away
  * [ ] Backfill (only what comes after signing in is bridged)
  * [ ] Room metadata changes after the room was made
  * [ ] Presence
* Misc
  * [x] Password and access-token sign-in, with the bridge's own device and keys
  * [x] Signing out ends the device there and deletes its keys
  * [x] New DMs by Matrix ID (`create_dm`), reusing one the account already has
  * [x] Joining a room by `#alias:server` or room ID (`POST /v3/join`)
  * [x] Rooms that are themselves bridges there are left out
  * [ ] Accepting invitations there
  * [ ] Cross-signing and key backup (the bridge's device is not verified)
