// Package library defines where synced songs live on the card, the manifest of what is synced,
// the .m3u writer, and the one guard every delete goes through.
package library

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

const maxComponent = 120

// SafeName makes s a single FAT-safe path component: reserved and control characters become
// "_", trailing dots and spaces go, and it is cut to 120 bytes on a UTF-8 boundary.
func SafeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, strings.ToValidUTF8(s, "_"))
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

func ext(s subsonic.Song, quality string) string {
	if quality == "original" && s.Suffix != "" {
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

// SongPath is <root>/Navidrome/<AlbumArtist>/<Year - >Album/<Disc->Track - Title.ext. It is pure;
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
	return filepath.Join(root, "Navidrome", SafeName(artist), SafeName(album), SafeName(file)+"."+ext(s, quality))
}

// WithID inserts " [<id>]" before the extension, cutting the stem so the name stays <=120 bytes.
func WithID(path, id string) string {
	e := filepath.Ext(path)
	dir, stem := filepath.Split(strings.TrimSuffix(path, e))
	tag := " [" + id + "]"
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
