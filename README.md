# Signal for KOReader

A Signal client for KOReader on a jailbroken Kindle. It links to your Signal
account as a **linked device**, the same way Signal Desktop does: the Kindle
shows a QR code, you scan it with Signal on your phone, and the Kindle appears
under *Linked devices*.

## Install

1. Download `signal.koplugin-vX.Y.Z.zip` from
   [Releases](https://github.com/rcdri/signal.koplugin/releases), unzip it and
   copy the `signal.koplugin` folder into `koreader/plugins/` on the Kindle.
   (A plain clone of this repository has no `bin/` helper; use a release.)
2. Restart KOReader.
3. Turn on Wi‑Fi and check the Kindle's date and time (Signal's encryption and
   TLS fail with a wrong clock).
4. Open **Tools (wrench) → Signal → Open Signal**. You can also put it on a
   gesture: *Gesture manager → General → Open Signal*.

## Link

1. On your phone open Signal → tap your profile picture → **Linked devices** →
   **Link a new device**.
2. Scan the code on the Kindle. Signal changes the code every 45 seconds; the
   Kindle follows automatically. After a few minutes it stops: tap **New code**.
3. When the phone asks whether to **transfer message history**, say yes if you
   want your recent chats on the Kindle (up to the last 300 messages per chat are
   kept). Otherwise the Kindle starts empty and fills up as messages arrive.

The device name shown on the phone is "Kindle (KOReader)"; change it in
*Signal → Advanced → Device name* before linking.

## Use

- **Chat list**: unread counts, previews, pinned chats first. Hold a chat to
  pin, mute, archive or clear it. Swipe or use ◀ ▶ to change page.
- **Chat**: newest messages at the bottom; ▲ / ▼ (or swipe) for older/newer.
  Tap a photo to view it, tap a file to save it to `Signal/` in your home
  folder (books and documents can be opened straight away). Hold any message
  for **Reply**, **React**, **Copy text**, **Edit** (your own, within 24 h),
  **Delete** (on the Kindle, or for everyone) and **Details**.
- **Writing**: *Write* opens the keyboard. Kindle fonts can't draw emoji, so
  they appear as names like `:thumbsup:`; typing `:name:` sends the real emoji
  (the ☺ button inserts common ones).
- **Delivery ticks**: ◷ sending, ✓ sent, ✓✓ delivered, ✓✓ read, ⚠ not sent
  (tap to retry).
- **Notifications** appear briefly while you read. Muted chats stay quiet.
- **Disappearing messages** are honoured: they are deleted from the Kindle when
  their timer runs out.
- **New chat** lists your contacts, or finds someone by phone number or Signal
  username.

## Settings (Tools → Signal)

| Setting | |
|---|---|
| Stay connected while reading | On by default. Keeps the helper running so messages and notifications arrive while you read. Turn it off to save battery; Signal then only connects while it's open. |
| Notifications / message text | Pop‑ups, and whether they show the message text. |
| Show photos in chats | Downloads photos (up to 8 MB) and shows them inline. |
| Text size | Message text size. |
| Advanced | Helper log, restart, reconnect, device name, helper on another computer. |
| Unlink this Kindle | Removes the Kindle from your account and deletes its data. |

## How it works

Signal's protocol needs real cryptography (libsignal), so the plugin starts a
small helper program, `bin/sig-helper-armhf`, and talks to it over
`127.0.0.1`. The helper keeps the encrypted connection to Signal, stores keys
and messages in `koreader/settings/signal/`, and exits when KOReader does.
It is built on **signalmeow**, the Signal library behind the mautrix-signal
bridge, and Signal's own **libsignal**. One static ARM binary works on every
Kindle that runs KOReader (soft‑ or hard‑float firmware).

**Helper on another computer**: the same binary runs on a Raspberry Pi
(32‑ or 64‑bit OS):

    ./sig-helper-armhf -data ~/signal -listen 0.0.0.0:18782 -token SOMESECRET

Then set the address and token in *Advanced → Use a helper on another computer*.
The Kindle then only needs Wi‑Fi while Signal is open.

## Good to know

- **Unofficial client.** Signal doesn't officially support third‑party clients,
  though it has long tolerated linked-device clients such as signal-cli and the
  mautrix bridge, which this uses.
- **History**: linked devices only receive messages sent after linking, plus
  whatever the phone transfers at link time. There is no "load more from phone".
- **Not supported**: calls (you get an "incoming call" notice), playing voice
  notes or videos (save them instead), stories, sending photos/files, polls
  (shown, vote on the phone).
- Pin, mute and archive are Kindle‑only; they don't change your phone.
- Signal removes linked devices that stay offline for about 30 days. If that
  happens, the plugin offers to link again.
- The helper uses roughly 40–60 MB of RAM and the binary is about 45 MB.

## License

AGPL-3.0, like KOReader, signalmeow (mautrix-signal) and libsignal.

## Building and releasing

The helper's source is in [`helper/`](helper/), with build instructions in
[`helper/README.md`](helper/README.md).

To publish a release, run **Actions → Release → Run workflow** and enter a
version (e.g. `1.0.1`). The workflow builds libsignal and the static ARM helper,
runs the helper's unit tests under qemu, packages `signal.koplugin-v1.0.1.zip`
with checksums, and creates the `v1.0.1` tag and release. It can also make a
draft or a pre-release. The mautrix-signal commit it builds against is pinned
as `MAUTRIX_REF` in `.github/workflows/release.yml`.
