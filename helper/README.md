# sig-helper

The Signal side of signal.koplugin. It links to a Signal account as a
secondary device (QR code), keeps the connection, stores messages in SQLite and
serves a small JSON API on localhost for the KOReader plugin.

Built on [signalmeow](https://github.com/mautrix/signal/tree/main/pkg/signalmeow)
(mautrix-signal) and Signal's [libsignal](https://github.com/signalapp/libsignal).
`backupconv.go` is adapted from mautrix-signal's `msgconv/from-signal-backup.go`.

## Build (static ARMv7, for Kindles)

Needs Go ≥ 1.26, Rust (the toolchain libsignal pins), protoc, and zig
(`pip install ziglang cargo-zigbuild`).

1. Check out [mautrix/signal](https://github.com/mautrix/signal) at the commit
   pinned as `MAUTRIX_REF` in `.github/workflows/release.yml`, next to this
   folder (`../mautrix-signal`, see `go.mod`), with its libsignal submodule.
2. Build libsignal-ffi: `./build-libsignal.sh`. It applies two small fixes:
   a cast in boring's `x509/verify.rs` (32-bit musl), and
   `patches/signalmeow-fat-fallocate.patch`, without which attachment downloads
   fail on the Kindle's FAT storage ("failed to pre-allocate file").
3. Build zlib for `arm-linux-musleabihf` and put `libz.a`, an empty
   `libstdc++.a` and `libsignal_ffi.a` in `$CROSS/arm/lib` (the script does this).
4. `VERSION=1.2.3 ./build-arm.sh` → `sig-helper-armhf`. `TAGS=demo ./build-arm.sh` adds a
   `-demo` flag: a fake account with sample chats, for testing the plugin
   without Signal.

The result is a static binary (musl, ARMv7 + VFPv3-D16) that runs on any Kindle
firmware and on Raspberry Pi OS, 32- or 64-bit.

`python3 tools/gen_emoji.py` (needs `pip install emoji`) regenerates the emoji
name table. Unit tests: build with `go test -c` for ARM and run under qemu.

## Run

    sig-helper -data DIR [-listen 127.0.0.1:18782] [-token SECRET] [-parent-pid PID]

| Flag | Meaning |
|---|---|
| `-data` | Folder for `signal.db` (keys, contacts, messages) and cached media. |
| `-listen` | API address. A non-localhost address requires `-token`. |
| `-token` | Clients must send `Authorization: Bearer SECRET`. |
| `-parent-pid` | Exit when this process ends (the plugin passes KOReader's PID). |
| `-device-name` | Name shown in the phone's Linked devices list. |
| `-mem-limit` | Soft Go memory limit in MB (default 96). |
| `-log` | DEBUG, INFO, WARN or ERROR. |

`HTTPS_PROXY` is honoured if set.

## API

All responses are JSON. Send `X-Emoji: names` to get emoji as `:names:`;
`:names:` in sent text always become emoji.

| Endpoint | |
|---|---|
| `GET /status` | state (unpaired, pairing, linking, connecting, connected, disconnected, logged_out, error), qr (`sgnl://linkdevice…`), linked, syncing (history, messages), me |
| `POST /login` | Start linking. The QR URL appears in `/status` and rotates every 45 s (6 times). |
| `POST /login-cancel`, `/logout`, `/reconnect`, `/shutdown` | |
| `GET /chats[?archived=1]`, `GET /chat?chat=` | Chats with unread counts and previews, pinned first. |
| `GET /messages?chat=&limit=&before_ts=&before_id=` | Oldest first; `more` says if older ones exist. |
| `POST /send {chat, text, reply_to?, retry_id?}` | Returns the pending message at once; delivery comes as events. |
| `POST /edit {chat,id,text}`, `/delete {chat,id,for_everyone}`, `/react {chat,id,emoji}` | Reacting with your current emoji removes it. |
| `POST /read {chat}` | Read receipts (if enabled on the account) and sync to the phone. |
| `POST /chat-flag {chat, pinned?, muted?, archived?}`, `/clear-chat {chat}` | Local only. |
| `POST /open {chat}`, `POST /lookup {phone \| username}`, `GET /contacts` | Start chats. |
| `GET /media?chat=&id=&idx=[&max=PX][&cached=1]` | Attachment bytes. `max` scales images to grayscale JPEG; `cached=1` answers 202 and downloads in the background (then a `media` event). |
| `GET /events?since=SEQ&wait=SECONDS` | msg, remove, chat, chats, names, media, typing, reaction, state, qr, error. `reset: true` means reload everything. |
