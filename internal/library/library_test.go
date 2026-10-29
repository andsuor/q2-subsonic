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
	if want := "/mnt/mmc/Subsonic/Art/1999 - Alb/03 - Intro.flac"; got != want {
		t.Fatal(got)
	}
	if got := SongPath("/mnt/mmc", song, "mp3-320", false); !strings.HasSuffix(got, "03 - Intro.mp3") {
		t.Fatal(got)
	}
}

func TestSongPathMultiDisc(t *testing.T) {
	s := song
	s.Disc = 2
	if got, want := SongPath("/r", s, "original", true), "/r/Subsonic/Art/1999 - Alb/2-03 - Intro.flac"; got != want {
		t.Fatal(got)
	}
}

func TestSongPathNoYear(t *testing.T) {
	s := song
	s.Year = 0
	if got, want := SongPath("/r", s, "original", false), "/r/Subsonic/Art/Alb/03 - Intro.flac"; got != want {
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
		root + "/Subsonic/../Music/x.flac",
		root + "/_explaylist_data/Other.m3u",
		root + "/Subsonic",
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
	os.MkdirAll(root+"/Subsonic", 0o755)
	if err := os.Symlink(out, root+"/Subsonic/link"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, root+"/Subsonic/link/f.flac"); err == nil {
		t.Fatal("symlink escape allowed")
	}
	if _, err := os.Stat(out + "/f.flac"); err != nil {
		t.Fatal("escaped file removed")
	}
}

func TestOwnedOutsideRoot(t *testing.T) {
	if Owned(t.TempDir(), "/etc/passwd", nil) {
		t.Fatal("owned")
	}
}

func TestOwnedAllows(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{root + "/Subsonic/A/B/1.mp3", root + "/_explaylist_data/Navidrome - X.m3u", root + "/.subsonic/log.txt"} {
		if !Owned(root, p, nil) {
			t.Fatal(p)
		}
	}
}

func TestRemovePrunesEmptyAlbum(t *testing.T) {
	root := t.TempDir()
	a, b := root+"/Subsonic/Art/1999 - Alb/01 - a.flac", root+"/Subsonic/Art/1999 - Alb/02 - b.flac"
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
	if _, err := os.Stat(root + "/Subsonic/Art"); !os.IsNotExist(err) {
		t.Fatal("empty folders left")
	}
	if _, err := os.Stat(root + "/Subsonic"); err != nil {
		t.Fatal("Navidrome itself removed")
	}
}

func TestM3UKeepsDuplicates(t *testing.T) {
	root := t.TempDir()
	p := PlaylistFile(root, "Mix: 1")
	if p != root+"/_explaylist_data/Mix_ 1.m3u" {
		t.Fatal(p)
	}
	m := &Manifest{Playlists: map[string]string{"Mix: 1": p}}
	if err := m.WriteM3U(root, p, []string{"/mnt/mmc/a.flac", "/mnt/mmc/b.flac", "/mnt/mmc/a.flac"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "#EXTM3U\n/mnt/mmc/a.flac\n/mnt/mmc/b.flac\n/mnt/mmc/a.flac\n" {
		t.Fatalf("%q", b)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/.subsonic/manifest.json"
	m, err := LoadManifest(p)
	if err != nil || len(m.Songs) != 0 {
		t.Fatal(err, m)
	}
	m.Songs["1"] = &Entry{ID: "1", Path: "/x", Quality: "original", Size: 5, Items: []string{"album:a"}, Meta: "h"}
	if err := m.Save(dir, p); err != nil {
		t.Fatal(err)
	}
	m2, err := LoadManifest(p)
	if err != nil || m2.Songs["1"].Meta != "h" || m2.Songs["1"].Items[0] != "album:a" {
		t.Fatal(err, m2)
	}
}

func TestRemoveSymlinkDotDotRefused(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	mk(t, filepath.Join(filepath.Dir(out), "f"))
	defer os.Remove(filepath.Join(filepath.Dir(out), "f"))
	os.MkdirAll(root+"/Subsonic", 0o755)
	os.Symlink(out, root+"/Subsonic/link")
	mk(t, root+"/Subsonic/f")
	if err := Remove(root, root+"/Subsonic/link/../f"); err == nil {
		t.Fatal("allowed")
	}
	for _, p := range []string{filepath.Join(filepath.Dir(out), "f"), root + "/Subsonic/f"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(p, "gone")
		}
	}
}

func TestMetaKeyArtistFallback(t *testing.T) {
	a, b := song, song
	a.AlbumArtist, b.AlbumArtist = "", ""
	b.Artist = "Someone Else"
	if MetaKey(a, "original") == MetaKey(b, "original") {
		t.Fatal("Artist change not detected")
	}
}

func TestWritesRefuseOutsideOwned(t *testing.T) {
	root := t.TempDir()
	if err := (&Manifest{}).WriteM3U(root, root+"/Music/x.m3u", []string{"a"}); err == nil {
		t.Fatal("m3u")
	}
	if err := (&Manifest{}).WriteM3U(root, root+"/_explaylist_data/Other.m3u", nil); err == nil {
		t.Fatal("m3u other")
	}
	m := &Manifest{Songs: map[string]*Entry{}}
	if err := m.Save(root, root+"/elsewhere/manifest.json"); err == nil {
		t.Fatal("save")
	}
	for _, d := range []string{"Music", "_explaylist_data", "elsewhere"} {
		if _, err := os.Stat(root + "/" + d); err == nil {
			t.Fatal(d, "created")
		}
	}
}

func TestWithIDKeeps120(t *testing.T) {
	stem := strings.Repeat("é", 59) + "a" // 119 bytes
	got := WithID("/r/"+stem+".flac", "12345")
	name := filepath.Base(got)
	if len(name) > 120 || !utf8.ValidString(name) || !strings.HasSuffix(name, " [12345].flac") {
		t.Fatal(len(name), name)
	}
}

func TestPlaylistFileNoPrefix(t *testing.T) {
	if got := PlaylistFile("/r", "Andreas"); got != "/r/_explaylist_data/Andreas.m3u" {
		t.Fatal(got)
	}
}

func TestOwnedPlaylistsOnlyRecorded(t *testing.T) {
	root := t.TempDir()
	rec, other := PlaylistFile(root, "Mine"), PlaylistFile(root, "Other")
	m := &Manifest{Playlists: map[string]string{"Mine": rec}}
	if !Owned(root, rec, m) || Owned(root, other, m) || Owned(root, rec, nil) {
		t.Fatal("recorded only")
	}
	if err := Remove(root, other); err == nil {
		t.Fatal("removed unrecorded")
	}
	if err := m.WriteM3U(root, other, nil); err == nil {
		t.Fatal("wrote unrecorded")
	}
}
