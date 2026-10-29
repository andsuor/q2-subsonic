# q2-subsonic

A Subsonic client for the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod). Pick playlists, albums, artists and songs on the Q2, and it syncs them to the card over Wi-Fi, or streams a song straight from your server. It needs a Q2 Pod build that supports card apps, and a Subsonic or OpenSubsonic server (Navidrome, for one) reachable over HTTPS.

## Install

**Over Wi-Fi:** on the Q2, open **System settings → Apps** and press **Subsonic** (once it is listed there).

**By hand:**

1. Download `q2-subsonic-*.zip` from the latest release.
2. Power the Q2 off, then put its microSD card in your computer.
3. Unzip the file to the card's root, so the card has `.subsonic/q2-subsonic`, `.subsonic/run`, `.subsonic/q2app` and its icons `.subsonic/icon.png` and `.subsonic/icon-40.png`. The leading dot hides the folder on macOS (Cmd+Shift+. in Finder shows it) and Linux.
4. Eject the card, put it back in the Q2 and power it on.

**Subsonic** then has a row in **Streaming**.

## Sign in

1. Open **Streaming → Subsonic**. While signed out, the first row is **Sign In from Your Phone**.
2. Press it. The Q2 shows a QR code and a PIN. Scan the code with your phone on the same Wi-Fi (or open the address and type the PIN) and fill in the server address, user name and password.
3. The Q2 checks the login and the list opens.

Things to know:

- The sign-in page only runs while you are signed out, and for at most 10 minutes per start. Leave the page and it stops.
- The password crosses your local Wi-Fi unencrypted, once, from the phone to the Q2. Do it on a network you trust.
- The server must be `https://`; `http://` is refused. Certificates are verified.
- The password is stored on the card, in `.subsonic/credentials`. exFAT has no file permissions, so anyone with the card can read it. **Make a server user just for the Q2, not an admin.** If the card is lost, delete that user on the server and the card is worthless.
- **Settings → Account → Sign Out** removes the stored login. Your music stays on the card.

## Use

The list has **Sync**, **On This Q2**, **Playlists**, **Albums**, **Artists**, **Search** and **Settings**.

**Choosing what to sync:** hold **Play/Pause** on a playlist, album, artist or song to toggle it in Sync, or open it and press **Add to Sync** / **Remove from Sync**. The Q2 answers `Added to Sync · N songs`, `Removed from Sync` or, for a song already covered by something bigger, `Included via <name>`. **On This Q2** lists what you picked, with its status and size on the card; hold Play/Pause there to take one out.

**Quality** is per item, with a default in **Settings → Default Quality**: **Original** (the default), **MP3 320** or **MP3 128**. AAC is not offered: Navidrome's AAC transcode can't carry tags.

**Syncing:** open **Sync**. It first shows a preview, and nothing happens until you press **Start Sync**: songs to add (`+N`), to remove (`−N`), to replace (`Replace N`), and `Needs X · Y free`. If the card is too small it says so and offers no Start. Then it shows progress: percent, songs, time left, the current song, and **Pause**, **Resume** and **Cancel**. Leave the page and the sync carries on. If Wi-Fi drops or the Q2 restarts, it picks up where it was. Songs that fail are listed under **Failed Songs**, with **Retry Failed**.

**Playing:** press a song to play it and the rest of the list after it. Songs already synced play from the card; the others stream. A song streams in its original format: FLAC and MP3 always, and other formats the Q2 plays too when the firmware has the `playlist` extension; without it they stream as MP3 320. **Settings → Streaming Quality** (Original, the default, MP3 320 or MP3 128) overrides this: anything but Original transcodes every streamed song to MP3. Streamed files are deleted as you go.

## Sync rules

- An artist includes albums added to the server later. A playlist follows the server, order and duplicates included. A song reached through several items is stored once.
- Songs land at `Subsonic/<Album Artist>/<Year> - <Album>/<Disc>-<Track> - <Title>.<ext>`, and each playlist as `_explaylist_data/<name>.m3u`.
- Deletions happen only after the downloads have finished, and only for what the sync put there.

## What it touches

Only `Subsonic/`, the playlist `.m3u` files it wrote and `.subsonic/`. An older `Navidrome/` folder is moved to `Subsonic/` once, at start-up. Nothing else on the card is read or changed.

## Library and firmware extensions

Newer Q2 Pod firmware offers extensions to card apps. Without them everything still works, a little less smoothly:

| Extension | With it | Without it |
|---|---|---|
| `library` | After a sync the Q2 updates its library and imports the playlists itself (it waits while music is playing). A playlist you made on the Q2 is never replaced: a server playlist with the same name arrives as "<name> (Subsonic)". | Run **Library → Update Local Music**, then **Playlists → Import playlist** yourself. The sync page's **Update Your Library** row reminds you. |
| `progress` | A real progress bar. | A text bar such as `[#####-----] 47 %`. |
| `playlist` | Streaming plays the synced songs from the card inside a playlist, and streams every format the Q2 plays as it is. | Everything in the list streams, synced or not, and only FLAC and MP3 stream as they are (others as MP3 320). |

## Errors and logs

A failure shows a short message on screen. Open its **Details** row for the operation, the HTTP status or Subsonic error code, and the time. The full trail is in `.subsonic/log.txt`; passwords and tokens are never logged.

## Update and remove

**System settings → Apps** shows **Update** when a newer release is out. By hand, replace the five files in `.subsonic` listed above. Your login, settings and selection are kept.

To remove it, press **Subsonic** twice in **System settings → Apps**, or delete `.subsonic`. Synced music stays in `Subsonic/`; delete that folder to free the space.

## Build

    contrib/shanlingq2/build.sh

needs Go 1.22 or later and python3 (no dependencies outside Go's standard library), and leaves the card files in `target/shanlingq2/.subsonic` and the release ZIP beside them. Pushing a tag `q2-vX.Y` that matches `cmd/q2-subsonic/main.go`'s version builds and publishes the release (`.github/workflows/release.yml`). More: [contrib/shanlingq2/README](contrib/shanlingq2/README).

## License

[MIT](LICENSE). Built from [q2-filebrowser](https://github.com/group-a-study/q2-filebrowser); see [NOTICE](NOTICE).
