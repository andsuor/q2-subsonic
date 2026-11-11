# Changelog

What changed in each release of q2-subsonic, the Subsonic app for Q2 Pod, newest first. Version X.Y is the release tag `q2-vX.Y`; Q2 Pod's App Store installs the latest one. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

A few features need a newer Q2 Pod. Each one says so, and what the app does on older Q2 Pod instead.

## [Unreleased]

### Added

- **Discover**, a new tab of the first page (a row of its list on older Q2 Pod): **Shuffle All**, **Recently Added**, **Recently Played**, **Most Played**, **Random Albums**, **Starred** and **Genres**. A genre lists its albums and has its own **Shuffle**. Recently Played and Most Played show what the server user has played, songs played from the app included (see Report Plays below).
- An artist's page has **Play**, **Shuffle**, **Radio** (songs like the artist's) and **Top Songs**. On a server without Radio or Top Songs (Navidrome has both, through Last.fm) they say **Not available on this server**.
- A long list no longer ends in **More…** after 500 rows. It ends in **Next page**, which shows the next part in its place, with **Previous page** to go back, and Back leaves the list at once. Requires Q2 Pod 1.7.3 or later; on older Q2 Pod long lists keep More…, as before.
- **Artists** opens on the server's index (the letters A to Z on Navidrome) when you have more than 60 artists, each letter with its count. Requires Q2 Pod 1.7.3 or later; on older Q2 Pod, or a server that names no index, Artists stays one list.
- **Albums** has a **Sort** row: **Recently Added**, **By Artist** or **By Year** (newest first). The order is not remembered: Albums opens A to Z. Requires Q2 Pod 1.7.6 or later; on older Q2 Pod Albums is A to Z only.
- **Albums**, **Playlists**, Discover's album lists and a genre's albums show each one's cover beside its row. Pages that mix albums with other rows, such as an artist's page, Search and On This Q2, keep their plain rows. The list opens at once and its covers appear a few seconds later; in a long list, covers past the first rows appear the next time you open it. Covers come slower while a sync runs. Requires Q2 Pod 1.7.3 or later; on older Q2 Pod the lists look as before. A server that sends no cover, or one in WebP, leaves the rows plain.
- Songs you play from the app are reported to your server. It shows the song as playing now, and counts a play once the song has been on for half its length or 4 minutes, so play counts and a Last.fm or ListenBrainz account linked on the server follow what you hear on the Q2. A play that can't be sent is tried again with the next song. **Settings → Report Plays** turns it off. A song paused early still counts, as the Q2 doesn't tell the app about a pause. Songs played from the Q2's own Library are not reported. If the Q2 also uploads its own scrobbles to the same account, turn one of the two off.
- Songs you stream are kept on the card, in a stream cache, so a song streamed before starts at once, and a list you started plays on without Wi-Fi as far as its songs are kept. **Settings → Stream Cache** sets its size: 512 MB, 1 GB, 2 GB (the default), 4 GB, 8 GB or **Off**; each press takes the next one. When it is full, the songs played longest ago go first, and it gives way until 256 MB of the card are free. **Settings → Clear Cache** shows how much is kept and empties it on a second press; the list that is playing stays. A sync comes first: it takes the cache's space when it needs it, and the Sync page says so (`Takes 1.2 GB from the Stream Cache`). A kept song belongs to the quality it was streamed in, so another **Streaming Quality** fetches it again, and **Sign Out** empties the cache, but for a list that is still playing. Needs the firmware's `playlist` extension; without it, and with Stream Cache off, streamed files are deleted as you go, as before.
- **Go to Album** and **Go to Artist**, in Now Playing's hold menu, work on a song you stream: they open that album or artist in the app, where you can play it or add it to your sync. Requires Q2 Pod 1.6.0 or later; on older Q2 Pod nothing changes. Songs you have synced open the Library's own pages, as before.

### Changed

- The first page is now tabs: **Home** (the whole list as before, so everything is still reachable with the wheel), **Discover**, **Playlists**, **Albums** and **Artists**. Swipe left or right to move between them; the title says which one you are on, such as Albums 4/5. A skipped or failed song is reported on Home. Requires Q2 Pod 1.7.3 or later; on older Q2 Pod the first page is one list, as before.
- **Search** is a magnifying glass at the right of the title bar on every tab; tap it to search. Under **Theme: Classic**, which has no title bar, it is the first row of each tab. Requires Q2 Pod 1.7.3 or later; on older Q2 Pod Search stays a row in the list.
- A song pressed in search results plays on through the songs found, instead of alone.
- With **Sync Entire Library** on, the "Checking server…" step of Sync is faster. The app asks for the songs 500 at a time instead of album by album. When the server says its library hasn't changed since the last check, less than a day ago, only the playlists are checked again (Navidrome says so; the songs it found are kept in `.subsonic/library.json`, for a library of up to 20,000 songs). A server that can't do either is checked album by album, as before. **Check Again** always checks everything.
- While it checks, the Sync page counts, such as `4500 of 10000 songs`, then `3 of 12 playlists`. Once the server has answered it says `Working out the changes…` while it compares them with the card, which takes a while with a large library.
- The check no longer gives up after 10 minutes on a large library. It stops when the server stops answering.
- The check looks through the card for songs deleted from it once each time the app starts and when you press **Check Again**, not at every check: on a large library that look took most of the time. A song you delete on the Q2 while the app runs is synced again after **Check Again** or the next start.
- Artists, Playlists, a long playlist or album and search results are fetched from the server once, not once for every 50 rows, so long lists fill faster. A song pressed on a page you just opened starts without fetching its list again. A change made on the server can take a minute to show.

### Fixed

- A streamed list no longer ends when Wi-Fi drops for a while. The songs already fetched play on, the app keeps trying the next one, and the list goes on when the server answers again. Before, the list ended about 40 seconds after the server was lost, even if Wi-Fi came back. It still ends if no song has started for 30 minutes.
- An internal error while answering one page no longer ends the app, and with it a running sync or stream. The page says "Something went wrong" with Retry; the error goes to `.subsonic/log.txt`.
- **Default Quality** and **Streaming Quality** say "Couldn't save the setting" when the card can't be written, instead of showing a quality that wasn't saved.
- Sync's **Last synced** is when the last sync finished. Before, a check of the server could move it, though nothing was synced. On a card synced with an older version it is right from the next sync on.
- Albums and artists show **On Q2**, or how many of their songs are on the Q2, as soon as the app starts. Before, Albums and Recently Added showed nothing for them until Sync had checked the server. Songs synced with an older version show it again after the next check.

## [0.5] - 2026-11-07

### Fixed

- A chosen album, artist, playlist or song that the server no longer has no longer stops the whole sync with "Album not found". This happens when the server's library is reorganised and the item gets a new id. The rest of your selection now syncs. The missing item reads **Not on the server** in On This Q2, and its songs stay on the Q2 until you remove it there. If the server can't be reached, the sync still stops, so songs are never removed because of a network problem.

## [0.4] - 2026-10-30

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

[Unreleased]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.5...HEAD
[0.5]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.4...q2-v0.5
[0.4]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.3...q2-v0.4
[0.3]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.2...q2-v0.3
[0.2]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.1...q2-v0.2
[0.1]: https://github.com/andsuor/q2-subsonic/releases/tag/q2-v0.1
