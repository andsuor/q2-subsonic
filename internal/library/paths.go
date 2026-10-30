// Package library defines where synced songs live on the card, the manifest of what is synced,
// the .m3u writer, and the one guard every delete goes through.
package library

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

const maxComponent = 120

// dotRun is two or more dots: the firmware refuses ".." anywhere in a playlist or queue line.
var dotRun = regexp.MustCompile(`\.{2,}`)

// SafeName makes s a single FAT-safe path component: reserved and control characters become
// "_", a run of dots becomes "…" and a leading dot "_" (the Q2's scan skips hidden names),
// trailing dots and spaces go, and it is cut to 120 bytes on a UTF-8 boundary.
func SafeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, strings.ToValidUTF8(s, "_"))
	s = dotRun.ReplaceAllString(s, "…")
	if strings.HasPrefix(s, ".") {
		s = "_" + s[1:]
	}
	return cutName(s, maxComponent)
}

// cutName cuts s to at most n bytes on a UTF-8 boundary, then drops trailing dots and spaces; "_"
// when nothing is left.
func cutName(s string, n int) string {
	if len(s) > n {
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	if s = strings.TrimRight(s, ". "); s == "" {
		return "_"
	}
	return s
}

// safeExt: a server-supplied suffix is used only if it can only be an extension, 1-5 of [a-z0-9].
// Anything else (e.g. "x/../../.subsonic/q2-subsonic") can't steer the path.
var safeExt = regexp.MustCompile(`^[a-z0-9]{1,5}$`)

// SafeSuffix reports whether a song's suffix can name its original file. The plan fetches a song
// without one as mp3-320, so the "mp3" ext() falls back to is true.
func SafeSuffix(suffix string) bool { return safeExt.MatchString(strings.ToLower(suffix)) }

// playable are the formats the Q2 plays: demo's own scan list, checked on the device (R40), and
// alac, which some servers give ALAC in .m4a.
var playable = map[string]bool{"mp3": true, "wav": true, "wma": true, "ape": true, "flac": true, "aiff": true, "aif": true,
	"m4a": true, "aac": true, "dff": true, "dsf": true, "ogg": true, "dts": true, "mp2": true, "ac3": true, "opus": true,
	"tak": true, "alac": true}

// Playable reports whether a song's original, by its suffix, plays on the Q2 (and so can be its
// file's extension).
func Playable(suffix string) bool { return playable[strings.ToLower(suffix)] }

func ext(s subsonic.Song, quality string) string {
	if quality == "original" && SafeSuffix(s.Suffix) {
		return strings.ToLower(s.Suffix)
	}
	return "mp3"
}

func artistOf(s subsonic.Song) string {
	if s.AlbumArtist != "" {
		return s.AlbumArtist
	}
	if s.Artist != "" {
		return s.Artist
	}
	return "Unknown Artist"
}

// SongPath is <root>/Subsonic/<AlbumArtist>/<Year - >Album/<Disc->Track - Title.ext. It is pure;
// collisions are resolved by the caller with WithID.
func SongPath(root string, s subsonic.Song, quality string, multiDisc bool) string {
	artist := artistOf(s)
	album := s.Album
	if album == "" {
		album = "Unknown Album"
	}
	if s.Year > 0 {
		album = fmt.Sprintf("%d - %s", s.Year, album)
	}
	file := fmt.Sprintf("%02d - %s", s.Track, s.Title)
	if multiDisc {
		file = fmt.Sprintf("%d-%s", s.Disc, file)
	}
	return filepath.Join(root, "Subsonic", SafeName(artist), SafeName(album), SafeName(file)+"."+ext(s, quality))
}

// WithID inserts " [<id>]" before the extension, cutting the stem so the name stays <=120 bytes.
func WithID(path, id string) string {
	e := filepath.Ext(path)
	dir, stem := filepath.Split(strings.TrimSuffix(path, e))
	tag := " [" + SafeName(id) + "]"
	if n := maxComponent - len(e) - len(tag); len(stem) > n && n > 0 {
		for n > 0 && !utf8.RuneStart(stem[n]) {
			n--
		}
		stem = strings.TrimRight(stem[:n], ". ")
	}
	return dir + stem + tag + e
}

// MetaKey hashes the fields that build the path, so a metadata change is detectable.
func MetaKey(s subsonic.Song, quality string) string {
	h := sha1.Sum([]byte(strings.Join([]string{artistOf(s), fmt.Sprint(s.Year), s.Album,
		fmt.Sprint(s.Disc), fmt.Sprint(s.Track), s.Title, s.Suffix, quality}, "|")))
	return hex.EncodeToString(h[:])
}

// PlaylistSum is a short digest of a playlist's song paths, in order.
func PlaylistSum(paths []string) string {
	h := sha1.Sum([]byte(strings.Join(paths, "\n")))
	return hex.EncodeToString(h[:8])
}

// maxPlaylistName is the most of a playlist's name, in bytes, the Q2 keeps (char title[64]). It
// cuts a longer one when it creates the playlist, then looks playlists up by the whole name, so
// never finds that one again: every import would add a copy (issue #1).
const maxPlaylistName = 63

// PlaylistFile is <root>/_explaylist_data/<name>.m3u, the name the Q2's Import playlist shows.
func PlaylistFile(root, name string) string { return PlaylistFileN(root, name, 0) }

// PlaylistFileN is the n-th file name for playlist name, for a clash: n 0 is "<name>.m3u", 1
// "<name> (Subsonic).m3u", 2 "<name> (Subsonic 2).m3u"... The name before .m3u, suffix included,
// is cut to the maxPlaylistName bytes the Q2 keeps.
func PlaylistFileN(root, name string, n int) string {
	suffix := ""
	switch {
	case n == 1:
		suffix = " (Subsonic)"
	case n > 1:
		suffix = fmt.Sprintf(" (Subsonic %d)", n)
	}
	return filepath.Join(root, "_explaylist_data", cutName(SafeName(name), maxPlaylistName-len(suffix))+suffix+".m3u")
}

// StalePlaylistFile reports whether a recorded playlist file has a name PlaylistFileN no longer
// makes: one the Q2 refuses (an older SafeName's "..") or cuts (longer than it keeps). The sync
// moves it.
func StalePlaylistFile(f string) bool {
	b := filepath.Base(f)
	return strings.Contains(b, "..") || len(strings.TrimSuffix(b, ".m3u")) > maxPlaylistName
}

// PlaylistTitle is the name the Q2 gives the playlist imported from f: the file's name without
// .m3u, cut to maxPlaylistName bytes as the firmware cuts it (mid-character too).
func PlaylistTitle(f string) string {
	s := strings.TrimSuffix(filepath.Base(f), ".m3u")
	return s[:min(len(s), maxPlaylistName)]
}

// LegacyPlaylistFile is the old "Navidrome - <name>.m3u", kept only so a sync can migrate it.
func LegacyPlaylistFile(root, name string) string {
	return filepath.Join(root, "_explaylist_data", "Navidrome - "+SafeName(name)+".m3u")
}

// StaleName reports whether path, under <root>/Subsonic/, has a component SafeName no longer makes:
// one the Q2's scan hides (a leading ".") or its playlists refuse (".."). The plan moves such songs.
func StaleName(root, path string) bool {
	rel, ok := strings.CutPrefix(path, filepath.Join(root, "Subsonic")+string(filepath.Separator))
	if !ok {
		return false
	}
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(c, ".") || strings.Contains(c, "..") {
			return true
		}
	}
	return false
}
