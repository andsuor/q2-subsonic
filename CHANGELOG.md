# Changelog

What changed in each release of q2-subsonic, the Subsonic app for Q2 Pod, newest first. Version X.Y is the release tag `q2-vX.Y`; Q2 Pod's App Store installs the latest one. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

A few features need Q2 Pod 1.5.9 or later. Each one says so, and what the app does on older Q2 Pod instead.

## [Unreleased]

### Added

- **Settings** shows the app's version, to quote in a bug report.

### Fixed

- Stopping the app from the Q2 can no longer hang: if the app hasn't ended 2 s after it's asked to, the launcher kills it, so USB storage and the App Store don't wait on it.

## [0.3] - 2026-10-30

### Changed

- While you stream a list, a song the server can't send (gone from the server, or in a format it can't convert) is skipped and the songs after it play on. The main page says which song was skipped and why. If the server can't be reached or refuses your account, or three songs in a row fail, the stream still stops at that song. Requires Q2 Pod 1.5.9 or later; on older Q2 Pod the stream stops at such a song, as before.

## [0.2] - 2026-10-30

### Fixed

- A playlist whose name is longer than the 63 bytes the Q2 keeps no longer gets another copy on the Q2 at every sync: its file name is now cut to fit. The next sync moves such a playlist to the shorter name; copies an older version already left on the Q2 stay until you delete them. ([#1](https://github.com/andsuor/q2-subsonic/issues/1))

## [0.1] - 2026-10-29

First release. It needs Q2 Pod with card apps and a Subsonic or OpenSubsonic server (Navidrome, for one) reachable over HTTPS.

### Added

- Browse your server on the Q2 under **Streaming → Subsonic**: Playlists, Albums, Artists and Search.
- Long lists load in full. Requires Q2 Pod 1.5.9 or later; on older Q2 Pod a **More…** row at the end loads the next page.
- Press a song to play it and the rest of the list after it. Songs already synced play from the card, and the others stream in their own format when the Q2 plays it. Requires Q2 Pod 1.5.9 or later; on older Q2 Pod every song in the list streams, synced or not, and only FLAC and MP3 stream as they are, other formats as MP3 320.
- **Settings → Streaming Quality** streams the originals (the default) or converts every streamed song to MP3 320 or MP3 128.
- Sync playlists, albums, artists and single songs to the card over Wi-Fi: hold **Play/Pause** on one, or open it and press **Add to Sync**. **On This Q2** lists what you picked, with its status and size on the card.
- **Sync Entire Library**, the first row of the main list, syncs everything on the server.
- Every sync starts with a preview of the songs to add, remove and replace, the playlists to update, and the space it needs and has. Nothing happens until you press **Start Sync**, and a card that is too small gets no Start.
- Sync progress shows percent, songs, time left and the current song, with **Pause**, **Resume** and **Cancel**. The sync carries on when you leave the page, and picks up where it was after a Wi-Fi drop or a restart.
- A progress bar on the sync page. Requires Q2 Pod 1.5.9 or later; on older Q2 Pod it is text, such as `[#####-----] 47 %`.
- Songs that fail to sync are listed under **Failed Songs**, with **Retry Failed**.
- Quality per synced item: Original, MP3 320 or MP3 128, with a default in **Settings → Default Quality**. An original the Q2 can't play (WavPack, Musepack) syncs as MP3 320.
- Synced playlists keep their name, order and duplicates, and a synced artist includes albums added to the server later.
- After a sync the Q2 updates its library and imports the playlists by itself, waiting while music plays. A playlist you made on the Q2 is never replaced: a server playlist of the same name arrives with " (Subsonic)" after its name. Requires Q2 Pod 1.5.9 or later; on older Q2 Pod run **Library → Update Local Music** and **Playlists → Import playlist** yourself, as the sync page reminds you.
- Sign in from your phone: the Q2 shows a QR code and a PIN, and you enter the server address, user name and password on the phone.
- Sign in with your password, or with an OpenSubsonic API key in its place when the server offers them. With a key, only the key is stored on the card.
- **Settings → Account** shows the server and user, and **Sign Out** removes the stored login. Your music stays on the card.
- A failure shows a short message and a **Details** row with the operation, the HTTP status or Subsonic error code, and the time. The full trail is in `.subsonic/log.txt`, and if the app crashed, its trace is added there at the next start.

### Security

- Only `https://` servers, with certificates checked against a built-in list of trusted authorities. When the Q2's clock is unset, the time for that check comes from a server whose certificate checks out.
- The phone sign-in page opens a port only after you press **Sign In from Your Phone**. It needs the one-time key from the QR code or the PIN, locks for a minute after 10 wrong PINs in a row, and stops once you are signed in, when you leave the QR page, or after 10 minutes. The password crosses your Wi-Fi from the phone to the Q2 unencrypted, once: sign in on a network you trust.
- The password is never sent to the server: sign-in uses a salted token, or the API key.
- Passwords, tokens, API keys and a `user:password@` in the server address never reach `log.txt` or the Account page.
- The app changes only `Subsonic/`, the playlist files it wrote and `.subsonic/`, and deletes only what a sync put there, after the downloads have finished.

[Unreleased]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.3...HEAD
[0.3]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.2...q2-v0.3
[0.2]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.1...q2-v0.2
[0.1]: https://github.com/andsuor/q2-subsonic/releases/tag/q2-v0.1
