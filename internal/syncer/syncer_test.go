package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// fake is a Navidrome stand-in serving deterministic bytes per song id.
type fake struct {
	mu      sync.Mutex
	status  map[string]int  // id -> forced HTTP status
	drop    map[string]bool // id -> cut the next response at 50 %
	noRange bool            // answer 200 with the whole file even when asked for a Range
	block   map[string]chan struct{}
	ranges  map[string][]string // id -> Range headers seen
	hits    map[string]int
}

// seen returns the hits and Range headers for id.
func (f *fake) seen(id string) (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[id], append([]string(nil), f.ranges[id]...)
}

func data(id string) []byte { return bytes.Repeat([]byte("song-"+id+"|"), 7000) }

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	f.mu.Lock()
	f.hits[id]++
	f.ranges[id] = append(f.ranges[id], r.Header.Get("Range"))
	st, drop, block := f.status[id], f.drop[id], f.block[id]
	delete(f.drop, id)
	noRange := f.noRange
	f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/getCoverArt") {
		w.Write([]byte("jpeg-" + id))
		return
	}
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}
	if st != 0 {
		w.WriteHeader(st)
		return
	}
	b := data(id)
	if drop {
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.Write(b[:len(b)/2])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	if noRange {
		r.Header.Del("Range")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

func setup(t *testing.T) (*fake, *Runner, string) {
	t.Helper()
	f := &fake{status: map[string]int{}, drop: map[string]bool{}, block: map[string]chan struct{}{},
		ranges: map[string][]string{}, hits: map[string]int{}}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	c, err := subsonic.New(subsonic.Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	r := New(root, c, nil)
	r.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	r.CardMounted = func() bool { return true }
	return f, r, root
}

func want(root, id, quality string) plan.Want {
	s := subsonic.Song{ID: id, Title: "T" + id, Album: "Alb", AlbumArtist: "Art", Year: 2000, Disc: 1, Track: len(id),
		Suffix: "flac", CoverArt: "al-1"}
	return plan.Want{Song: s, Quality: quality, Path: library.SongPath(root, s, quality, false), Items: []string{"album:al"},
		Bytes: int64(len(data(id)))}
}

func wait(t *testing.T, r *Runner) Snapshot {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if s := r.Snapshot(); s.State != "running" {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("still running")
	return Snapshot{}
}

func manifest(t *testing.T, root string) *library.Manifest {
	t.Helper()
	b, err := os.ReadFile(root + "/.subsonic/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m := &library.Manifest{}
	if err := json.Unmarshal(b, m); err != nil {
		t.Fatal("manifest not valid JSON:", err)
	}
	return m
}

func sameFile(t *testing.T, path, id string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(b, data(id)) {
		t.Fatalf("%s: %v, %d bytes", path, err, len(b))
	}
}

func noParts(t *testing.T, root string) {
	t.Helper()
	filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error {
		if strings.HasSuffix(p, ".part") || strings.HasSuffix(p, ".tmp") {
			t.Errorf("left over: %s", p)
		}
		return nil
	})
}

func TestRunDownloadsAndRenames(t *testing.T) {
	_, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "mp3-320")
	old, moved := root+"/Navidrome/Old/x/01 - r.flac", root+"/Navidrome/Art/2000 - Alb/09 - r.flac"
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("r"), 0o644)
	m := &library.Manifest{Songs: map[string]*library.Entry{"r": {ID: "r", Path: old, Quality: "original", Size: 1}}}
	if err := m.Save(root, root+"/.subsonic/manifest.json"); err != nil {
		t.Fatal(err)
	}
	p := plan.Plan{Add: []plan.Want{a, b}, Rename: []plan.Move{{From: old, To: moved, ID: "r"}},
		Playlists: map[string][]string{"Mix": {a.Path, moved, a.Path}}}
	if err := r.Start(p); err != nil {
		t.Fatal(err)
	}
	s := wait(t, r)
	if s.State != "done" || s.Done != 2 || s.Total != 2 || s.Failed != 0 || s.Permille != 1000 {
		t.Fatalf("%+v", s)
	}
	sameFile(t, a.Path, "a")
	sameFile(t, b.Path, "bb")
	if _, err := os.Stat(moved); err != nil {
		t.Fatal("not renamed")
	}
	if _, err := os.Stat(root + "/Navidrome/Old"); !os.IsNotExist(err) {
		t.Fatal("old album folder left")
	}
	if c, err := os.ReadFile(filepath.Dir(a.Path) + "/cover.jpg"); err != nil || string(c) != "jpeg-al-1" {
		t.Fatal("cover", err)
	}
	noParts(t, root)
	mf := manifest(t, root)
	for _, id := range []string{"a", "bb", "r"} {
		if mf.Songs[id] == nil {
			t.Fatal("manifest misses", id)
		}
	}
	if mf.Songs["r"].Path != moved || mf.Songs["a"].Meta != library.MetaKey(a.Song, "original") ||
		mf.Songs["bb"].Quality != "mp3-320" || mf.Songs["a"].Size != int64(len(data("a"))) {
		t.Fatalf("%+v %+v", mf.Songs["r"], mf.Songs["a"])
	}
	m3u, _ := os.ReadFile(library.PlaylistFile(root, "Mix"))
	if string(m3u) != "#EXTM3U\n"+a.Path+"\n"+moved+"\n"+a.Path+"\n" {
		t.Fatalf("%q", m3u)
	}
	if _, err := os.Stat(root + "/.subsonic/pending.json"); !os.IsNotExist(err) {
		t.Fatal("pending.json left after done")
	}
}

func TestRunResumeAfterDrop(t *testing.T) {
	f, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "mp3-320")
	f.drop["a"], f.drop["bb"] = true, true
	r.Start(plan.Plan{Add: []plan.Want{a, b}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	half := len(data("a")) / 2
	_, ra := f.seen("a")
	_, rb := f.seen("bb")
	if got := ra; len(got) != 2 || got[0] != "" || got[1] != fmt.Sprintf("bytes=%d-", half) {
		t.Fatalf("ranges %q", got)
	}
	if got := rb; len(got) != 2 || got[1] != "" {
		t.Fatalf("transcoded resumed with Range: %q", got)
	}
	sameFile(t, a.Path, "a")
	sameFile(t, b.Path, "bb")
	noParts(t, root)
}

func TestRunRangeIgnoredRestarts(t *testing.T) {
	f, r, root := setup(t)
	a := want(root, "a", "original")
	f.drop["a"], f.noRange = true, true
	r.Start(plan.Plan{Add: []plan.Want{a}})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	sameFile(t, a.Path, "a")
}

func TestRunFailureDoesNotStop(t *testing.T) {
	f, r, root := setup(t)
	a, x, c := want(root, "a", "original"), want(root, "x", "original"), want(root, "ccc", "original")
	f.status["x"] = 404
	r.Start(plan.Plan{Add: []plan.Want{a, x, c}})
	s := wait(t, r)
	if hx, _ := f.seen("x"); s.State != "done" || s.Failed != 1 || s.Done != 2 || hx != 3 {
		t.Fatalf("%+v hits=%d", s, hx)
	}
	fl := s.Failures[0]
	if len(s.Failures) != 1 || fl.HTTP != 404 || fl.SongID != "x" || fl.Op != "download" || fl.Title != "Tx" ||
		fl.At.IsZero() || !strings.Contains(fl.Msg, "404") {
		t.Fatalf("%+v", s.Failures)
	}
	sameFile(t, c.Path, "ccc")
	if _, err := os.Stat(x.Path); !os.IsNotExist(err) {
		t.Fatal("failed song left a file")
	}

	f.mu.Lock()
	delete(f.status, "x")
	f.mu.Unlock()
	r.RetryFailed()
	if s := wait(t, r); s.State != "done" || s.Failed != 0 || s.Done != 3 {
		t.Fatalf("retry %+v", s)
	}
	sameFile(t, x.Path, "x")
	if ha, _ := f.seen("a"); ha != 1 {
		t.Fatal("finished song fetched again")
	}
}

func TestRunCardGone(t *testing.T) {
	_, r, root := setup(t)
	a, b, c := want(root, "a", "original"), want(root, "bb", "original"), want(root, "ccc", "original")
	var mounted atomic.Bool
	mounted.Store(true)
	var checks atomic.Int32
	r.CardMounted = func() bool {
		if checks.Add(1) > 2 { // the card goes after the first song
			mounted.Store(false)
		}
		return mounted.Load()
	}
	p := plan.Plan{Add: []plan.Want{a, b, c}}
	r.Start(p)
	s := wait(t, r)
	if s.State != "paused" || !errors.Is(s.Err, q2.ErrNoCard) {
		t.Fatalf("%+v", s)
	}
	m := manifest(t, root)
	for id, e := range m.Songs { // whatever the manifest names is whole
		sameFile(t, e.Path, id)
	}
	if m.Songs["ccc"] != nil {
		t.Fatal("manifest names a song never fetched")
	}
	if _, err := os.Stat(root + "/.subsonic/pending.json"); err != nil {
		t.Fatal("pending plan lost", err)
	}

	r.CardMounted = func() bool { return true }
	r.Resume()
	if s := wait(t, r); s.State != "done" || s.Done != 3 || s.Err != nil {
		t.Fatalf("%+v", s)
	}
	for _, w := range p.Add {
		sameFile(t, w.Path, w.Song.ID)
	}
}

func TestDeletesAfterDownloads(t *testing.T) {
	_, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "original")
	z := &library.Entry{ID: "z", Path: root + "/Navidrome/Z/z/01 - z.flac", Quality: "original"}
	os.MkdirAll(filepath.Dir(z.Path), 0o755)
	os.WriteFile(z.Path, []byte("z"), 0o644)
	os.WriteFile(filepath.Dir(z.Path)+"/cover.jpg", []byte("c"), 0o644)
	stale := library.PlaylistFile(root, "Gone")
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("#EXTM3U\n"), 0o644)
	m := &library.Manifest{Songs: map[string]*library.Entry{"z": z}}
	m.Save(root, root+"/.subsonic/manifest.json")
	var mu sync.Mutex
	var ops []string
	r.onOp = func(op, path string) {
		mu.Lock()
		ops = append(ops, op)
		mu.Unlock()
	}
	r.Start(plan.Plan{Add: []plan.Want{a, b}, Remove: []*library.Entry{z}, Playlists: map[string][]string{}})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	if strings.Join(ops, ",") != "download,download,remove" {
		t.Fatal(ops)
	}
	if _, err := os.Stat(root + "/Navidrome/Z"); !os.IsNotExist(err) {
		t.Fatal("removed song's folder left")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale playlist left")
	}
	if manifest(t, root).Songs["z"] != nil {
		t.Fatal("removed song still in manifest")
	}
}

func TestProgressPermilleAndETA(t *testing.T) {
	if p := permille(500, 1000); p != 500 {
		t.Fatal(p)
	}
	if p := permille(2000, 1000); p != 1000 {
		t.Fatal(p)
	}
	t0 := time.Unix(1000, 0)
	var m meter
	m.add(t0, 0)
	for i := 1; i <= 9; i++ { // 100 kB/s for 9 s
		m.add(t0.Add(time.Duration(i)*time.Second), 100_000)
	}
	if e := m.eta(t0.Add(9*time.Second), 1_000_000); e != 0 {
		t.Fatal("ETA before 10 s of data:", e)
	}
	for i := 10; i <= 60; i++ {
		m.add(t0.Add(time.Duration(i)*time.Second), 100_000)
	}
	if e := m.eta(t0.Add(60*time.Second), 1_000_000); e < 9*time.Second || e > 11*time.Second {
		t.Fatal("ETA", e)
	}

	_, r, _ := setup(t)
	clock := t0
	r.Now = func() time.Time { return clock }
	r.mu.Lock()
	r.state, r.bytesTotal, r.bytesDone = "running", 1000, 500
	r.mu.Unlock()
	if s := r.Snapshot(); s.Permille != 500 || s.ETA != 0 || s.BytesDone != 500 {
		t.Fatalf("%+v", s)
	}
}

func TestCancelKeepsFinished(t *testing.T) {
	f, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "original")
	f.block["bb"] = make(chan struct{})
	p := plan.Plan{Add: []plan.Want{a, b}}
	r.Start(p)
	for i := 0; ; i++ {
		if h, _ := f.seen("bb"); h > 0 {
			break
		}
		if i > 1000 {
			t.Fatal("bb never requested")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !r.Busy() || r.Start(p) != ErrBusy {
		t.Fatal("not busy")
	}
	r.Cancel()
	if s := r.Snapshot(); s.State != "idle" || r.Busy() {
		t.Fatalf("%+v", s)
	}
	sameFile(t, a.Path, "a")
	m := manifest(t, root)
	if m.Songs["a"] == nil || m.Songs["bb"] != nil {
		t.Fatalf("%+v", m.Songs)
	}
	noParts(t, root)
	if _, err := os.Stat(root + "/.subsonic/pending.json"); !os.IsNotExist(err) {
		t.Fatal("pending.json kept after cancel")
	}
}

func TestNetSlotPriority(t *testing.T) {
	s := NewNetSlot()
	ctx := context.Background()
	s.Acquire(ctx, false)
	got := make(chan string, 2)
	go func() { s.Acquire(ctx, false); got <- "normal"; s.Release() }()
	time.Sleep(20 * time.Millisecond)
	go func() { s.Acquire(ctx, true); got <- "prio"; s.Release() }()
	time.Sleep(20 * time.Millisecond)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if s.Acquire(cctx, false) == nil {
		t.Fatal("acquired with a cancelled context while held")
	}
	s.Release()
	if a, b := <-got, <-got; a != "prio" || b != "normal" {
		t.Fatal(a, b)
	}
	if err := s.Acquire(ctx, false); err != nil {
		t.Fatal(err)
	}
}
