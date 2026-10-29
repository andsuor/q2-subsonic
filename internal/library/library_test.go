package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

func TestSafeNameReserved(t *testing.T) {
	if got := SafeName("AC/DC: Live?"); got != "AC_DC_ Live_" {
		t.Fatal(got)
	}
	if got := SafeName("a\x01b<>\"\\|*"); got != "a_b______" {
		t.Fatal(got)
	}
}

func TestSafeNameTrailingDotsSpaces(t *testing.T) {
	if got := SafeName("Hello. . "); got != "Hello" {
		t.Fatal(got)
	}
	if got := SafeName(".."); got != "_" {
		t.Fatal(got)
	}
}

func TestSafeNameUTF8Cut(t *testing.T) {
	for _, s := range []string{strings.Repeat("æøå", 34), strings.Repeat("日本語", 22)} {
		got := SafeName(s)
		if len(got) > 120 || !utf8.ValidString(got) || len(got) < 110 {
			t.Fatalf("%d bytes valid=%v", len(got), utf8.ValidString(got))
		}
	}
}

var song = subsonic.Song{ID: "1", Title: "Intro", Album: "Alb", AlbumArtist: "Art", Artist: "Other", Year: 1999, Disc: 1, Track: 3, Suffix: "flac"}

func TestSongPathSingleDisc(t *testing.T) {
	got := SongPath("/mnt/mmc", song, "original", false)
	if want := "/mnt/mmc/Navidrome/Art/1999 - Alb/03 - Intro.flac"; got != want {
		t.Fatal(got)
	}
	if got := SongPath("/mnt/mmc", song, "mp3-320", false); !strings.HasSuffix(got, "03 - Intro.mp3") {
		t.Fatal(got)
	}
}

func TestSongPathMultiDisc(t *testing.T) {
	s := song
	s.Disc = 2
	if got, want := SongPath("/r", s, "original", true), "/r/Navidrome/Art/1999 - Alb/2-03 - Intro.flac"; got != want {
		t.Fatal(got)
	}
}

func TestSongPathNoYear(t *testing.T) {
	s := song
	s.Year = 0
	if got, want := SongPath("/r", s, "original", false), "/r/Navidrome/Art/Alb/03 - Intro.flac"; got != want {
		t.Fatal(got)
	}
}

func TestWithID(t *testing.T) {
	if got := WithID("/a/b/03 - x.flac", "42"); got != "/a/b/03 - x [42].flac" {
		t.Fatal(got)
	}
}

func TestMetaKey(t *testing.T) {
	a := MetaKey(song, "original")
	s := song
	s.Title = "Outro"
	if a == MetaKey(s, "original") || a == MetaKey(song, "mp3-128") || a != MetaKey(song, "original") {
		t.Fatal("MetaKey")
	}
	s = song
	s.Duration = 99 // not a path field
	if a != MetaKey(s, "original") {
		t.Fatal("MetaKey sensitive to non-path field")
	}
}

func mk(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveRefusesOutside(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		root + "/Music/x.flac",
		root + "/Navidrome/../Music/x.flac",
		root + "/_explaylist_data/Other.m3u",
		root + "/Navidrome",
	} {
		mk(t, filepath.Clean(p))
		if err := Remove(root, p); err == nil {
			t.Fatalf("no error for %s", p)
		}
		if _, err := os.Stat(filepath.Clean(p)); err != nil {
			t.Fatalf("%s gone", p)
		}
	}
	// symlink escape
	root = t.TempDir()
	out := t.TempDir()
	mk(t, out+"/f.flac")
	os.MkdirAll(root+"/Navidrome", 0o755)
	if err := os.Symlink(out, root+"/Navidrome/link"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, root+"/Navidrome/link/f.flac"); err == nil {
		t.Fatal("symlink escape allowed")
	}
	if _, err := os.Stat(out + "/f.flac"); err != nil {
		t.Fatal("escaped file removed")
	}
}

func TestOwnedOutsideRoot(t *testing.T) {
	if Owned(t.TempDir(), "/etc/passwd") {
		t.Fatal("owned")
	}
}

func TestOwnedAllows(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{root + "/Navidrome/A/B/1.mp3", root + "/_explaylist_data/Navidrome - X.m3u", root + "/.subsonic/log.txt"} {
		if !Owned(root, p) {
			t.Fatal(p)
		}
	}
}

func TestRemovePrunesEmptyAlbum(t *testing.T) {
	root := t.TempDir()
	a, b := root+"/Navidrome/Art/1999 - Alb/01 - a.flac", root+"/Navidrome/Art/1999 - Alb/02 - b.flac"
	mk(t, a)
	mk(t, b)
	if err := Remove(root, a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(b)); err != nil {
		t.Fatal("album removed while non-empty")
	}
	if err := Remove(root, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root + "/Navidrome/Art"); !os.IsNotExist(err) {
		t.Fatal("empty folders left")
	}
	if _, err := os.Stat(root + "/Navidrome"); err != nil {
		t.Fatal("Navidrome itself removed")
	}
}

func TestM3UKeepsDuplicates(t *testing.T) {
	root := t.TempDir()
	p := PlaylistFile(root, "Mix: 1")
	if p != root+"/_explaylist_data/Navidrome - Mix_ 1.m3u" {
		t.Fatal(p)
	}
	if err := WriteM3U(p, []string{"/mnt/mmc/a.flac", "/mnt/mmc/b.flac", "/mnt/mmc/a.flac"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "#EXTM3U\n/mnt/mmc/a.flac\n/mnt/mmc/b.flac\n/mnt/mmc/a.flac\n" {
		t.Fatalf("%q", b)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/sub/manifest.json"
	m, err := LoadManifest(p)
	if err != nil || len(m.Songs) != 0 {
		t.Fatal(err, m)
	}
	m.Songs["1"] = &Entry{ID: "1", Path: "/x", Quality: "original", Size: 5, Items: []string{"album:a"}, Meta: "h"}
	if err := m.Save(p); err != nil {
		t.Fatal(err)
	}
	m2, err := LoadManifest(p)
	if err != nil || m2.Songs["1"].Meta != "h" || m2.Songs["1"].Items[0] != "album:a" {
		t.Fatal(err, m2)
	}
}
