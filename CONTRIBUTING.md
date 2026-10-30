# Contributing

Thanks for helping. Bug reports and feature requests go in [Issues](https://github.com/andsuor/q2-subsonic/issues); changes come as pull requests against `master`.

## Setup

Go 1.22 or later and python3. The app uses Go's standard library only: please don't add dependencies.

```sh
go vet ./...
gofmt -l .                                  # must print nothing; gofmt -w fixes it
go test -race ./...
go test ./internal/syncer -run TestRunCardGone    # one test
python3 contrib/shanlingq2/test_launcher.py # the card's run script
contrib/shanlingq2/build.sh                 # the Q2 build (static MIPS, soft float)
```

CI runs all of these on every pull request.

## Trying a build on a Q2

`build.sh` leaves `target/shanlingq2/q2-subsonic-vX.Y-SHA.zip`. Power the Q2 off and unzip it to the root of its card, so the card has `.subsonic/q2-subsonic`, `.subsonic/run` and the rest, then power the Q2 on. Your login, settings and selection in `.subsonic/` are kept. The log is `.subsonic/log.txt`.

## Layout

In the order data flows:

| Path | Role |
|---|---|
| `internal/subsonic` | API client: token auth, typed errors, URL redaction |
| `internal/selection` | `.subsonic/selection.json`, what the user chose |
| `internal/plan` | The sync preview: what to add, replace, rename and remove |
| `internal/syncer` | Runs a sync: `.part` files renamed when done, resume, deletes last |
| `internal/library` | Card layout `Subsonic/…`, the manifest, `.m3u` files and the ownership guard: every card write or delete goes through it |
| `internal/fw` | The firmware's capabilities and the library update request |
| `cmd/q2-subsonic` | The daemon: Q2 screens (`device*.go`), phone sign-in, streaming |
| `contrib/shanlingq2` | Build, packaging and the card's launcher |

The app is a Q2 Pod card app: the firmware draws every screen from the rows the app sends over its socket (Q2SPOT/1, see Q2 Pod's `docs/librespot.md`).

## Changelog

Every change a user can notice adds a bullet under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md), in the right subsection (`### Added`, `### Changed`, `### Fixed`, `### Removed`, `### Security`). Write it for someone using the app on a Q2: what they see or can now do, not function names. One bullet per change. If it needs a firmware capability (`playlist`, `progress`, `library`, `pages` in `/tmp/q2-addon.caps`), end the bullet with "Requires Q2 Pod 1.5.9 or later" (or the version that added the capability) and what the app does on older Q2 Pod.

Changes users can't notice (tests, refactors, CI) need no bullet: label the pull request `no changelog`. CI checks this.

## Commits

`type(scope): summary`, in plain words and lower case, as in `git log`:

```
fix(library): cap playlist names at the 63 bytes the Q2 keeps
feat(stream): skip a song the server can't send and play on
```

Types: `feat`, `fix`, `docs`, `test`, `refactor`, `ci`. The scope is the package or area (`stream`, `library`, `syncer`, `subsonic`, `signin`, …) and can be left out. The body says why, and `Fixes #N` closes an issue.

## Releasing

Maintainers, from an up-to-date `master` with a clean tree:

```sh
contrib/release.sh 0.4 --dry-run   # checks, tests, and shows the notes and the diff
contrib/release.sh 0.4             # commit "release: q2-v0.4" and tag q2-v0.4
git push --atomic origin master q2-v0.4    # or run the script with --push
```

The script moves the Unreleased entries into `## [0.4] - <today>`, updates the links at the bottom of CHANGELOG.md and sets `const version` in `cmd/q2-subsonic/main.go`. The tag's workflow checks the version, builds, and publishes the release with that section as its notes and the ZIP as its only file. Q2 Pod's App Store installs the latest release's ZIP, so keep the `q2-vX.Y` tags and the single ZIP.

## Licence

MIT, see [LICENSE](LICENSE); contributions are under the same licence. `internal/q2spot`, `internal/q2` and `internal/qr` are copied verbatim from [q2-filebrowser](https://github.com/group-a-study/q2-filebrowser) (MIT, see [NOTICE](NOTICE)); keep them as they are there. Never copy code from q2-deezer, which is AGPL.
