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

	"github.com/andsuor/q2-subsonic/internal/applog"
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
	slow    map[string]bool     // id -> send in 32 KiB writes, 3 ms apart
	ranges  map[string][]string // id -> Range headers seen
	hits    map[string]int
}

// seen returns the hits and Range headers for id.
func (f *fake) seen(id string) (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[id], append([]string(nil), f.ranges[id]...)
}

func data(id string) []byte {
	if strings.HasPrefix(id, "big") {
		return bytes.Repeat([]byte("song-"+id+"|"), 300000)
	}
	return bytes.Repeat([]byte("song-"+id+"|"), 7000)
}

type slowWriter struct{ http.ResponseWriter }

func (w slowWriter) Write(b []byte) (int, error) {
	time.Sleep(3 * time.Millisecond)
	n, err := w.ResponseWriter.Write(b)
	w.ResponseWriter.(http.Flusher).Flush()
	return n, err
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	f.mu.Lock()
	f.hits[id]++
	f.ranges[id] = append(f.ranges[id], r.Header.Get("Range"))
	st, drop, block, slow := f.status[id], f.drop[id], f.block[id], f.slow[id]
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
	if slow {
		w = slowWriter{w}
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

func setup(t *testing.T) (*fake, *Runner, string) {
	t.Helper()
	f := &fake{status: map[string]int{}, drop: map[string]bool{}, block: map[string]chan struct{}{}, slow: map[string]bool{},
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
	r.Net = NewNetSlot()
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
	m := &library.Manifest{Songs: map[string]*library.Entry{"z": z}, Playlists: map[string]string{"Gone": stale}}
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

func TestRunFullSizePartRecovers(t *testing.T) {
	f, r, root := setup(t)
	a, b := want(root, "a", "original"), want(root, "bb", "original")
	b.Bytes = 0 // size unknown: the full .part asks for a Range past the end and gets 416
	for _, w := range []plan.Want{a, b} {
		os.MkdirAll(filepath.Dir(w.Path), 0o755)
		os.WriteFile(w.Path+".part", bytes.Repeat([]byte("x"), len(data(w.Song.ID))), 0o644)
	}
	r.Start(plan.Plan{Add: []plan.Want{a, b}})
	if s := wait(t, r); s.State != "done" || s.Failed != 0 {
		t.Fatalf("%+v", s)
	}
	sameFile(t, a.Path, "a")
	sameFile(t, b.Path, "bb")
	if _, ra := f.seen("a"); len(ra) != 1 || ra[0] != "" {
		t.Fatalf("full .part at planned size sent Range: %q", ra)
	}
	if _, rb := f.seen("bb"); len(rb) != 2 || rb[0] != fmt.Sprintf("bytes=%d-", len(data("bb"))) || rb[1] != "" {
		t.Fatalf("416 not recovered in one try: %q", rb)
	}
	noParts(t, root)
}

func TestStreamerPreemptsMidFile(t *testing.T) {
	f, r, root := setup(t)
	big := want(root, "big", "original")
	f.slow["big"] = true
	r.Start(plan.Plan{Add: []plan.Want{big}})
	for i := 0; r.Snapshot().BytesDone == 0; i++ {
		if i > 1000 {
			t.Fatal("download never started")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Net.Acquire(ctx, true); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	if s.State != "running" || s.BytesDone >= s.BytesTotal {
		t.Fatalf("streamer served only after the file: %+v", s)
	}
	time.Sleep(50 * time.Millisecond)
	if b := r.Snapshot().BytesDone; b != s.BytesDone {
		t.Fatal("download kept going while the streamer held the slot", b, s.BytesDone)
	}
	r.Net.Release()
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	sameFile(t, big.Path, "big")
	if _, rg := f.seen("big"); len(rg) != 1 {
		t.Fatalf("download restarted: %q", rg)
	}
}

func TestRestartResumes(t *testing.T) {
	f, ra, root := setup(t)
	a, big := want(root, "a", "original"), want(root, "big", "original")
	f.slow["big"] = true
	p := plan.Plan{Add: []plan.Want{a, big}}
	ra.Start(p)
	for i := 0; ; i++ {
		if s := ra.Snapshot(); s.Done == 1 && s.BytesDone > a.Bytes {
			break
		}
		if i > 2000 {
			t.Fatal("big never started")
		}
		time.Sleep(time.Millisecond)
	}
	ra.Pause() // the process dies mid-file: the .part and pending.json stay
	st, err := os.Stat(big.Path + ".part")
	if err != nil || st.Size() == 0 {
		t.Fatal("no .part", err)
	}

	rb := New(root, ra.src, nil)
	rb.Backoff, rb.CardMounted, rb.Net = ra.Backoff, ra.CardMounted, NewNetSlot()
	if err := rb.Start(p); err != nil {
		t.Fatal(err)
	}
	if s := wait(t, rb); s.State != "done" || s.Done != 2 {
		t.Fatalf("%+v", s)
	}
	if h, _ := f.seen("a"); h != 1 {
		t.Fatal("finished song fetched again", h)
	}
	if _, rg := f.seen("big"); len(rg) != 2 || rg[1] != fmt.Sprintf("bytes=%d-", st.Size()) {
		t.Fatalf("ranges %q, part %d", rg, st.Size())
	}
	sameFile(t, big.Path, "big")
}

func TestLogRedacted(t *testing.T) {
	_, r, root := setup(t)
	r.log = applog.Open(root+"/log.txt", 1<<20)
	r.logf("sync: %v", errors.New("GET https://h/rest/download?u=u&t=tok123&s=salt99&p=pw77 -> EOF"))
	b, _ := os.ReadFile(root + "/log.txt")
	if len(b) == 0 || strings.Contains(string(b), "tok123") || strings.Contains(string(b), "salt99") ||
		strings.Contains(string(b), "pw77") {
		t.Fatalf("%q", b)
	}
}

// syncPlaylists runs a sync of the given playlists (name -> no songs) over a card prepared by prep.
func syncPlaylists(t *testing.T, prep func(root string), names ...string) (string, *library.Manifest) {
	t.Helper()
	_, r, root := setup(t)
	if prep != nil {
		prep(root)
	}
	pl := map[string][]string{}
	for _, n := range names {
		pl[n] = nil
	}
	r.Start(plan.Plan{Playlists: pl})
	if s := wait(t, r); s.State != "done" {
		t.Fatalf("%+v", s)
	}
	return root, manifest(t, root)
}

func put(t *testing.T, p, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUserPlaylistNotOverwritten(t *testing.T) {
	var user string
	root, m := syncPlaylists(t, func(root string) {
		user = library.PlaylistFile(root, "Andreas")
		put(t, user, "mine")
	}, "Andreas")
	if b, _ := os.ReadFile(user); string(b) != "mine" {
		t.Fatalf("user file changed: %q", b)
	}
	want := root + "/_explaylist_data/Andreas (Navidrome).m3u"
	if _, err := os.Stat(want); err != nil || m.Playlists["Andreas"] != want {
		t.Fatal(err, m.Playlists)
	}
}

func TestRecordedPlaylistRewrittenAndStaleOnlyRecorded(t *testing.T) {
	var other, gone string
	root, m := syncPlaylists(t, func(root string) {
		gone = library.PlaylistFile(root, "Gone")
		other = library.PlaylistFile(root, "Other")
		put(t, gone, "x")
		put(t, other, "user")
		put(t, library.PlaylistFile(root, "Keep"), "old")
		mm := &library.Manifest{Playlists: map[string]string{"Gone": gone, "Keep": library.PlaylistFile(root, "Keep")}}
		mm.Save(root, root+"/.subsonic/manifest.json")
	}, "Keep")
	if b, _ := os.ReadFile(other); string(b) != "user" {
		t.Fatal("unrecorded Other.m3u touched")
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatal("recorded stale file left")
	}
	if b, _ := os.ReadFile(library.PlaylistFile(root, "Keep")); string(b) != "#EXTM3U\n" {
		t.Fatalf("Keep not rewritten in place: %q", b)
	}
	if len(m.Playlists) != 1 || m.Playlists["Keep"] != library.PlaylistFile(root, "Keep") {
		t.Fatal(m.Playlists)
	}
}

func TestLegacyPrefixMigrated(t *testing.T) {
	var legacySel, legacyGone string
	root, m := syncPlaylists(t, func(root string) {
		legacySel = library.LegacyPlaylistFile(root, "Mix")
		legacyGone = library.LegacyPlaylistFile(root, "Old")
		put(t, legacySel, "#EXTM3U\n")
		put(t, legacyGone, "#EXTM3U\n")
	}, "Mix")
	for _, f := range []string{legacySel, legacyGone} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatal("legacy file left:", f)
		}
	}
	if _, err := os.Stat(library.PlaylistFile(root, "Mix")); err != nil || m.Playlists["Mix"] != library.PlaylistFile(root, "Mix") {
		t.Fatal(err, m.Playlists)
	}
}
