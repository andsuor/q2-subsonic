package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func legacyRig(t *testing.T) (root string, m *Manifest) {
	root = t.TempDir()
	a, b := root+"/Navidrome/Art/Alb/01 - a.flac", root+"/Navidrome/Art/Alb/02 - b.flac"
	put(t, a, "A")
	put(t, b+".part", "B") // cut mid-download
	put(t, root+"/Navidrome/Art/Alb/cover.jpg", "c")
	put(t, root+"/.subsonic/pending.json", "{}")
	put(t, root+"/_explaylist_data/Mix.m3u", "#EXTM3U\n"+a+"\n")
	m = &Manifest{Songs: map[string]*Entry{"a": {ID: "a", Path: a}}, Playlists: map[string]string{"Mix": root + "/_explaylist_data/Mix.m3u"}}
	if err := m.Save(root, root+"/.subsonic/manifest.json"); err != nil {
		t.Fatal(err)
	}
	return
}

func TestMigrateFastPath(t *testing.T) {
	root, _ := legacyRig(t)
	if err := Migrate(root, t.Logf); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Subsonic/Art/Alb/01 - a.flac", "Subsonic/Art/Alb/02 - b.flac.part", "Subsonic/Art/Alb/cover.jpg"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Error(err)
		}
	}
	if _, err := os.Stat(root + "/Navidrome"); !os.IsNotExist(err) {
		t.Error("Navidrome/ still there")
	}
	if _, err := os.Stat(root + "/.subsonic/pending.json"); !os.IsNotExist(err) {
		t.Error("pending.json kept")
	}
	m, _ := LoadManifest(root + "/.subsonic/manifest.json")
	if got := m.Songs["a"].Path; got != root+"/Subsonic/Art/Alb/01 - a.flac" || !m.PlaylistsStale {
		t.Errorf("manifest path %q stale=%v", got, m.PlaylistsStale)
	}
	if err := Migrate(root, t.Logf); err != nil { // nothing left to do
		t.Fatal(err)
	}
}

func TestMigrateSlowPath(t *testing.T) {
	root, _ := legacyRig(t)
	put(t, root+"/Subsonic/Other/x.flac", "x")
	put(t, root+"/Navidrome/Art/Alb/notes.txt", "mine") // unknown: stays
	if err := Migrate(root, t.Logf); err != nil {
		t.Fatal(err)
	}
	m, _ := LoadManifest(root + "/.subsonic/manifest.json")
	if got := m.Songs["a"].Path; got != root+"/Subsonic/Art/Alb/01 - a.flac" {
		t.Errorf("manifest path %q", got)
	}
	for _, p := range []string{"Subsonic/Art/Alb/01 - a.flac", "Navidrome/Art/Alb/02 - b.flac.part", "Subsonic/Other/x.flac", "Navidrome/Art/Alb/notes.txt"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Error(err)
		}
	}
	if _, err := os.Stat(root + "/Navidrome/Art/Alb/01 - a.flac"); !os.IsNotExist(err) {
		t.Error("song left behind")
	}
	if _, err := os.Stat(root + "/.subsonic/pending.json"); !os.IsNotExist(err) {
		t.Error("pending.json kept")
	}
}

func TestMigrateFailureKeepsEverything(t *testing.T) {
	root, _ := legacyRig(t)
	before, _ := os.ReadFile(root + "/.subsonic/manifest.json")
	os.Chmod(root+"/.subsonic", 0o555) // the manifest can't be saved after the move
	defer os.Chmod(root+"/.subsonic", 0o755)
	if err := Migrate(root, t.Logf); err == nil {
		t.Skip("directory permissions not enforced (root?)")
	}
	if _, err := os.Stat(root + "/Navidrome/Art/Alb/01 - a.flac"); err != nil {
		t.Error("song lost: ", err)
	}
	if _, err := os.Stat(root + "/Subsonic"); !os.IsNotExist(err) {
		t.Error("Subsonic/ left behind")
	}
	if after, _ := os.ReadFile(root + "/.subsonic/manifest.json"); string(after) != string(before) {
		t.Error("manifest changed")
	}
}

func TestGuardSubsonicTree(t *testing.T) {
	root := t.TempDir()
	if !Owned(root, root+"/Subsonic/A/1.mp3", nil) {
		t.Error("Subsonic/ not owned")
	}
	if Owned(root, root+"/Navidrome/A/1.mp3", nil) || Owned(root, root+"/Subsonic", nil) {
		t.Error("legacy tree or the folder itself owned")
	}
	p := root + "/Navidrome/A/1.mp3"
	put(t, p, "x")
	if err := Remove(root, p); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Error("removed from the legacy tree")
	}
}

func TestMigrateSlowPathRefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	put(t, out+"/x.flac", "outside")
	os.MkdirAll(root+"/Navidrome", 0o755)
	if err := os.Symlink(out, root+"/Navidrome/link"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(root+"/Subsonic", 0o755)
	m := &Manifest{Songs: map[string]*Entry{"x": {ID: "x", Path: root + "/Navidrome/link/x.flac"}}}
	if err := m.Save(root, root+"/.subsonic/manifest.json"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(root, t.Logf); err == nil {
		t.Error("no error")
	}
	if _, err := os.Stat(out + "/x.flac"); err != nil {
		t.Error("outside file moved")
	}
	if _, err := os.Stat(root + "/Subsonic/link"); !os.IsNotExist(err) {
		t.Error("destination created")
	}
}
