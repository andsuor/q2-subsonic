package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
)

func TestQ2AppName(t *testing.T) { // package.py zips this file as it is
	b, err := os.ReadFile("../../contrib/shanlingq2/q2app")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"name=Subsonic\n", "search=Search Subsonic\n"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("q2app lacks %q", w)
		}
	}
}

// The release builds with a current Go, not go.mod's minimum (M44).
func TestReleaseGoVersion(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "go-version-file") || !strings.Contains(string(b), "go-version: '1.27'") {
		t.Error("release.yml doesn't pin a current Go")
	}
}

func TestNoNavidromeInUI(t *testing.T) {
	check := func(what string, r q2spot.Reply) {
		t.Helper()
		all := append(titles(r), r.Message)
		for _, row := range r.Rows {
			all = append(all, row.Subtitle)
		}
		for _, s := range all {
			if strings.Contains(s, "Navidrome") {
				t.Errorf("%s shows %q", what, s)
			}
		}
	}
	for _, n := range []*nav{{albums: 3, songsPer: 2}, {empty: true}, {errCode: 40}} {
		d := newTestDaemon(t, n, "library", true)
		d.do("toggle", "album:al0")
		for _, op := range [][]string{{"menu"}, {"playlists"}, {"albums"}, {"artists"}, {"artist", "artist:ar1"}, {"search", "x"},
			{"playlist", "playlist:pl1"}, {"album", "album:al0"}, {"settings"}, {"account"}, {"onq2"}, {"library"},
			{"details", "getAlbum|0|0|0|"}} {
			check(strings.Join(op, " "), d.do(op[0], op[1:]...))
		}
		check("sync", d.do("sync"))
		check("sync", d.preview(t))
	}
	check("signed out", newTestDaemon(t, &nav{}, "", false).do("menu"))
	if b, _ := os.ReadFile("signin.html"); strings.Contains(string(b), "Navidrome") ||
		!strings.Contains(string(b), "<title>Sign in to your music server</title>") {
		t.Error("sign-in page title/heading")
	}
}

// legacyCard syncs al0 and playlist Mix, then turns the card into the old layout.
func legacyCard(t *testing.T) (card string, before string) {
	t.Helper()
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "album:al0")
	d.do("toggle", "playlist:pl1")
	d.preview(t)
	d.do("start")
	eventually(t, "the first sync", func() bool { return d.runner.Snapshot().State == "done" })
	card = d.card
	os.Rename(filepath.Join(card, "Subsonic"), filepath.Join(card, "Navidrome"))
	fix := func(p string) {
		b, _ := os.ReadFile(p)
		os.WriteFile(p, []byte(strings.ReplaceAll(string(b), card+"/Subsonic/", card+"/Navidrome/")), 0o644)
	}
	fix(filepath.Join(card, ".subsonic", "manifest.json"))
	m3us, _ := filepath.Glob(filepath.Join(card, "_explaylist_data", "*.m3u"))
	for _, f := range m3us {
		fix(f)
	}
	os.WriteFile(filepath.Join(card, ".subsonic", "pending.json"), []byte("{}"), 0o644)
	b, _ := os.ReadFile(m3us[0])
	return card, string(b)
}

func TestMigrateNavidromeFolder(t *testing.T) {
	card, before := legacyCard(t)
	if !strings.Contains(before, "/Navidrome/") {
		t.Fatal("rig: ", before)
	}
	d := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 2}, "", true)
	if _, err := os.Stat(filepath.Join(card, "Navidrome")); !os.IsNotExist(err) {
		t.Error("Navidrome/ remains")
	}
	m, _ := library.LoadManifest(filepath.Join(card, ".subsonic", "manifest.json"))
	for id, e := range m.Songs {
		if !strings.HasPrefix(e.Path, card+"/Subsonic/") {
			t.Errorf("%s at %s", id, e.Path)
		}
		if _, err := os.Stat(e.Path); err != nil {
			t.Error(err)
		}
	}
	d.do("toggle", "album:al0")
	r := d.preview(t) // the playlists' paths changed, so the sync is offered with nothing to download
	if _, ok := find(r, "Start Sync"); !ok {
		t.Fatalf("no sync offered: %q", titles(r))
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	m3us, _ := filepath.Glob(filepath.Join(card, "_explaylist_data", "*.m3u"))
	if len(m3us) != 1 {
		t.Fatalf("playlist files %v", m3us)
	}
	b, _ := os.ReadFile(m3us[0])
	if strings.Contains(string(b), "/Navidrome/") || !strings.Contains(string(b), card+"/Subsonic/") {
		t.Errorf("playlist after sync:\n%s", b)
	}
	if n := d.runner.Snapshot().Done; n != 0 {
		t.Errorf("%d downloads", n)
	}
}

func TestMigrateFailureShowsErrorRow(t *testing.T) {
	card, _ := legacyCard(t)
	os.Chmod(filepath.Join(card, ".subsonic"), 0o555)
	defer os.Chmod(filepath.Join(card, ".subsonic"), 0o755)
	d := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 2}, "", false) // the sign-in is on the card already
	if d.migrateErr == nil {
		t.Skip("directory permissions not enforced (root?)")
	}
	r := d.do("sync")
	if r.Rows[0].Title != "Couldn't move Navidrome/ to Subsonic/" {
		t.Fatalf("rows %q", titles(r))
	}
	if _, ok := find(r, "Details"); !ok {
		t.Error("no Details row")
	}
	if _, err := os.Stat(filepath.Join(card, "Navidrome")); err != nil {
		t.Error("Navidrome/ lost")
	}
}

func TestMigrateRecoversAfterCrash(t *testing.T) {
	card, _ := legacyCard(t)
	// the crash: the folder was renamed, the manifest still names Navidrome/
	if err := os.Rename(filepath.Join(card, "Navidrome"), filepath.Join(card, "Subsonic")); err != nil {
		t.Fatal(err)
	}
	d := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 2}, "", true)
	if d.migrateErr != nil {
		t.Fatal(d.migrateErr)
	}
	m, _ := library.LoadManifest(filepath.Join(card, ".subsonic", "manifest.json"))
	for id, e := range m.Songs {
		if !strings.HasPrefix(e.Path, card+"/Subsonic/") {
			t.Errorf("%s at %s", id, e.Path)
		}
	}
	r := d.do("sync")
	if len(r.Rows) > 0 && strings.HasPrefix(r.Rows[0].Title, "Couldn't") {
		t.Fatalf("error row %q", titles(r))
	}
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if n := d.runner.Snapshot().Done; n != 0 {
		t.Errorf("%d downloads", n)
	}
}

func TestMigrateLegacyPlaylistsRewritten(t *testing.T) {
	card, _ := legacyCard(t)
	dir := filepath.Join(card, "_explaylist_data")
	ms, _ := filepath.Glob(filepath.Join(dir, "*.m3u"))
	legacy := filepath.Join(dir, "Navidrome - Mix.m3u") // an older build: not recorded
	os.Rename(ms[0], legacy)
	mp := filepath.Join(card, ".subsonic", "manifest.json")
	m, _ := library.LoadManifest(mp)
	m.Playlists = map[string]string{}
	m.Save(card, mp)
	d := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 2}, "", true)
	r := d.preview(t)
	if _, ok := find(r, "Start Sync"); !ok {
		t.Fatalf("no sync offered: %q", titles(r))
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("legacy playlist kept")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "Mix.m3u"))
	if !strings.Contains(string(b), card+"/Subsonic/") || strings.Contains(string(b), "/Navidrome/") {
		t.Errorf("Mix.m3u:\n%s", b)
	}
}

func TestMigratedPartResumes(t *testing.T) {
	card, _ := legacyCard(t)
	mp := filepath.Join(card, ".subsonic", "manifest.json")
	m, _ := library.LoadManifest(mp)
	var song string
	for id, e := range m.Songs {
		song = e.Path
		delete(m.Songs, id)
		break
	}
	m.Save(card, mp)
	os.Remove(song)
	os.WriteFile(song+".part", []byte(strings.Repeat("x", 400)), 0o644) // cut mid-download, old tree
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemonOn(t, card, n, "", true)
	r := d.preview(t)
	if _, ok := find(r, "Start Sync"); !ok {
		t.Fatalf("no sync offered: %q", titles(r))
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if got, _ := n.lastRange.Load().(string); got != "bytes=400-" {
		t.Errorf("Range %q", got)
	}
	newPath := strings.Replace(song, "/Navidrome/", "/Subsonic/", 1)
	if st, err := os.Stat(newPath); err != nil || st.Size() != 1000 {
		t.Errorf("song %v %v", st, err)
	}
}
