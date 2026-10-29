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
	if len(s) > maxComponent {
		n := maxComponent
		for !utf8.RuneStart(s[n]) {
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

// PlaylistFile is <root>/_explaylist_data/<name>.m3u, the name the Q2's Import playlist shows.
func PlaylistFile(root, name string) string {
	return filepath.Join(root, "_explaylist_data", SafeName(name)+".m3u")
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
