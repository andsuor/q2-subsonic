package syncer

// Tests from the G1 review round, in their own file so the round merges without touching the end of
// syncer_test.go.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// The server down behind a proxy or CDN (Caddy's 502, Cloudflare's 52x, 503, 504; or 429) is the
// whole sync's problem, as no answer is: two songs in a row pause it (review G1, Important 1).
func TestProxyErrorPauses(t *testing.T) {
	f, r, root := setup(t)
	var ws []plan.Want
	for _, id := range []string{"a", "bb", "ccc", "dddd"} {
		f.status[id] = 502
		ws = append(ws, want(root, id, "original"))
	}
	r.Start(plan.Plan{Add: ws})
	s := wait(t, r)
	if s.State != "paused" || !errors.Is(s.Err, ErrOffline) {
		t.Fatalf("%+v", s)
	}
	if h, _ := f.seen("ccc"); h != 0 {
		t.Errorf("went on after two songs: %d requests for the third", h)
	}
	for _, st := range []int{429, 503, 504, 520, 527} {
		if err := stopReason(&subsonic.APIError{Op: "download", HTTP: st}); !errors.Is(err, ErrOffline) {
			t.Error(st, err)
		}
	}
	for _, st := range []int{404, 500} {
		if err := stopReason(&subsonic.APIError{Op: "download", HTTP: st}); err != nil {
			t.Error(st, "is the song's own:", err)
		}
	}
}

// Only an "exists" line naming exactly an add of that request is honoured. Any other (an import, a
// path in another form) is logged and ignored, and then no add of that request is recorded as
// imported: one of them may have been skipped, and recording it would let a later "import"
// replace the user's playlist. The guard fails closed (review G1, Minor 1).
func TestImportedTrustsOnlySentAdds(t *testing.T) {
	_, r, root := setup(t)
	mix, rock := library.PlaylistFile(root, "Mix"), library.PlaylistFile(root, "Rock")
	for _, f := range []string{mix, rock} {
		put(t, f, "#EXTM3U\n")
	}
	(&library.Manifest{Playlists: map[string]string{"Mix": mix, "Rock": rock}, Imported: map[string]bool{rock: true}}).Save(root, root+"/.subsonic/manifest.json")
	again, clash, err := r.Imported([2][]string{{rock}, {mix}}, []string{"/mnt/mmc/_explaylist_data/Mix.m3u", rock})
	if err != nil || len(again)+len(clash) != 0 {
		t.Fatal(again, clash, err)
	}
	m := manifest(t, root)
	if m.Imported[mix] || !m.Imported[rock] || m.Playlists["Mix"] != mix || m.Playlists["Rock"] != rock {
		t.Fatalf("%+v %+v", m.Imported, m.Playlists)
	}
}

// A rename onto a file no song records (one the app forgot in a crash window) replaces it instead
// of failing on every sync; one that another song records is never touched (review G1, Minor 3).
func TestRenameReplacesOrphan(t *testing.T) {
	_, r, root := setup(t)
	from, to := root+"/Subsonic/A/x/01 - r.flac", root+"/Subsonic/A/y/01 - r.flac"
	other, taken := root+"/Subsonic/A/z/01 - o.flac", root+"/Subsonic/A/z/01 - p.flac"
	put(t, from, "r")
	put(t, to, "orphan")
	put(t, other, "o")
	put(t, taken, "p")
	(&library.Manifest{Songs: map[string]*library.Entry{"r": {ID: "r", Path: from, Quality: "original"},
		"o": {ID: "o", Path: other, Quality: "original"}, "p": {ID: "p", Path: taken, Quality: "original"}}}).Save(root, root+"/.subsonic/manifest.json")
	r.Start(plan.Plan{Rename: []plan.Move{{From: from, To: to, ID: "r"}, {From: other, To: taken, ID: "o"}}})
	if s := wait(t, r); s.State != "done" || s.Failed != 1 || s.Failures[0].SongID != "o" {
		t.Fatalf("%+v", s)
	}
	if b, _ := os.ReadFile(to); string(b) != "r" {
		t.Errorf("to holds %q", b)
	}
	if b, _ := os.ReadFile(taken); string(b) != "p" {
		t.Errorf("a recorded song's file was replaced: %q", b)
	}
}

// An album folder left with only its cover and a download's leftovers (.part and .part.key of a
// plan that was replaced) is removed whole (review G1, Minor 7).
func TestRemovePrunesLeftoverParts(t *testing.T) {
	_, r, root := setup(t)
	z := &library.Entry{ID: "z", Path: root + "/Subsonic/Z/z/01 - z.flac", Quality: "original"}
	dir := filepath.Dir(z.Path)
	for _, f := range []string{z.Path, dir + "/cover.jpg", dir + "/02 - y.flac.part", dir + "/02 - y.flac.part.key"} {
		put(t, f, "x")
	}
	(&library.Manifest{Songs: map[string]*library.Entry{"z": z}}).Save(root, root+"/.subsonic/manifest.json")
	r.Start(plan.Plan{Remove: []*library.Entry{z}, Playlists: map[string][]string{}})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Stat(root + "/Subsonic/Z"); !os.IsNotExist(err) {
		es, _ := os.ReadDir(dir)
		t.Errorf("album folder left: %v", es)
	}
}
