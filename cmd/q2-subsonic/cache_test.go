package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/creds"
	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// cacheOn turns the stream cache on: limit bytes, account "acct", a card with free bytes free.
func (e *env) cacheOn(limit, free int64) {
	e.s.cacheCap = func() int64 { return limit }
	e.s.acct = func() string { return "acct" }
	e.s.free = func(string) int64 { return free }
	e.s.reserve = func() int64 { return 0 }
}

// kept is where song i of songs(n, ext) is cached at quality q.
func (e *env) kept(i int, ext, q string) string {
	sg := songs(i+1, ext)[i]
	return e.s.cachePath(e.s.cacheBase(), sg, ext, q)
}

// put writes a cache file of n bytes last played at day.
func put(t *testing.T, path string, n int, day int) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC)
	os.Chtimes(path, at, at)
}

func TestCachePath(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.cacheOn(1<<30, 1<<40)
	p := e.kept(0, "flac", "original")
	rel, _ := filepath.Rel(e.s.dir(), p)
	parts := strings.Split(rel, "/")
	if len(parts) != 4 || parts[0] != "acct" || parts[1] != "original" || len(parts[2]) != cacheShard || parts[3] != "t0 [s0].flac" {
		t.Fatalf("path %q", rel)
	}
	if streamID(p) != "s0" {
		t.Errorf("streamID(%q) = %q", p, streamID(p))
	}
	// FAT finds a file whatever its name's case: a title retagged in case alone is the same path,
	// the one the protected set and the firmware's folder listing have (review).
	if got := e.s.cachePath(e.s.cacheBase(), subsonic.Song{ID: "s0", Title: "t0"}, "flac", "original"); got != p {
		t.Errorf("a title in another case at %q, not %q", got, p)
	}
	if q := e.kept(0, "mp3", "mp3-128"); q == p || filepath.Base(filepath.Dir(filepath.Dir(q))) != "mp3-128" {
		t.Errorf("another quality at %q", q)
	}
	// An id that could share a name with another, or that a name can't hold whole, is not kept.
	for _, id := range []string{"a]b", "a [b", "a/b", "", strings.Repeat("x", 65), "é", "a..b", ".a", "a."} {
		if got := e.s.cachePath(e.s.cacheBase(), subsonic.Song{ID: id, Title: "T"}, "mp3", "original"); got != "" {
			t.Errorf("id %q cached at %q", id, got)
		}
	}
	e.s.cacheCap = func() int64 { return 0 } // Off
	if e.s.cacheBase() != "" {
		t.Error("a cache folder while Off")
	}
	e.cacheOn(1<<30, 1<<40)
	e.s.acct = func() string { return "" } // signed out
	if e.s.cacheBase() != "" {
		t.Error("a cache folder while signed out")
	}
	e.cacheOn(1<<30, 1<<40)
	e.s.caps.Playlist = false // folder mode plays one folder in name order
	if e.s.cacheBase() != "" {
		t.Error("a cache folder in folder mode")
	}
}

// A list played again fetches nothing, and nothing is deleted behind the song playing.
func TestCacheSecondPlayFetchesNothing(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.cacheOn(1<<30, 1<<40)
	list := songs(5, "flac")
	e.s.start(list, 0)
	until(t, "window", func() bool { return exists(e.kept(2, "flac", "original")) })
	_, body := e.queue(t)
	if !strings.Contains(body, e.kept(0, "flac", "original")+"\n") {
		t.Fatalf("queue %q", body)
	}
	e.setPlaying(t, e.kept(2, "flac", "original"))
	until(t, "the next window", func() bool { return exists(e.kept(4, "flac", "original")) })
	e.setPlaying(t, e.kept(4, "flac", "original"))
	until(t, "the rest", func() bool { return exists(e.kept(4, "flac", "original")) && !e.s.active() })
	for i := range list {
		if !exists(e.kept(i, "flac", "original")) {
			t.Fatalf("song %d is not kept", i)
		}
	}
	n := len(e.src.got())
	e.s.start(list, 0)
	e.setPlaying(t, e.kept(4, "flac", "original"))
	until(t, "the second run's end", func() bool { return !e.s.active() })
	if got := e.src.got(); len(got) != n {
		t.Fatalf("fetched again: %q", got[n:])
	}
	if e.s.used.Load() == 0 {
		t.Error("nothing counted as used")
	}
}

// Another Streaming Quality, or another account, never plays the file kept.
func TestCacheQualityAndAccountApart(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.cacheOn(1<<30, 1<<40)
	list := songs(1, "flac")
	e.s.start(list, 0)
	orig := e.kept(0, "flac", "original")
	until(t, "original", func() bool { return exists(orig) })
	e.s.quality = func() string { return "mp3-128" }
	e.s.start(list, 0)
	low := e.kept(0, "mp3", "mp3-128")
	until(t, "mp3-128", func() bool { return exists(low) })
	if _, body := e.queue(t); !strings.Contains(body, low+"\n") || strings.Contains(body, orig) {
		t.Fatalf("queue %q", body)
	}
	if e.src.tries("s0") != 2 || !exists(orig) {
		t.Fatalf("gets %q, original kept %v", e.src.got(), exists(orig))
	}
	e.s.acct = func() string { return "other" }
	e.s.start(list, 0)
	until(t, "the other account's", func() bool { return e.src.tries("s0") == 3 })
	if _, body := e.queue(t); !strings.Contains(body, "/stream/other/mp3-128/") {
		t.Fatalf("queue %q", body)
	}
}

// A half-fetched file is never taken as the song.
func TestCachePartNeverPlayed(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.cacheOn(1<<30, 1<<40)
	p := e.kept(0, "flac", "original")
	put(t, p+".part", 3, 1)
	e.s.start(songs(1, "flac"), 0)
	until(t, "fetched", func() bool { return exists(p) })
	if b, _ := os.ReadFile(p); string(b) != "data-p:s0:original" || exists(p+".part") {
		t.Fatalf("content %q, part left %v", b, exists(p+".part"))
	}
}

func TestCacheTrim(t *testing.T) {
	const mb = 1 << 20
	setup := func(limit, free int64) (*env, []string) {
		e := newEnv(t, fw.Caps{Playlist: true})
		e.cacheOn(1, free)
		var f []string
		for i := 0; i < 4; i++ {
			f = append(f, e.kept(i, "flac", "original"))
			put(t, f[i], 100, 10-i) // song 3 is the oldest
		}
		e.cacheOn(limit, free)
		return e, f
	}
	there := func(f []string) string {
		s := ""
		for _, p := range f {
			if exists(p) {
				s += "1"
			} else {
				s += "0"
			}
		}
		return s
	}
	// Over the limit: the songs played longest ago go, to a sixteenth under it.
	e, f := setup(400, 1<<40)
	e.s.trim(context.Background(), 100, 0)
	if got := there(f); got != "1100" || e.s.used.Load() != 200 {
		t.Errorf("over the limit: %s, used %d", got, e.s.used.Load())
	}
	// The list playing is never taken, nor a song of it half fetched.
	e, f = setup(400, 1<<40)
	put(t, f[3]+".part", 50, 1)
	e.s.protect([]string{f[3], f[2]})
	e.s.trim(context.Background(), 100, 0)
	if got := there(f); got != "0011" || !exists(f[3]+".part") {
		t.Errorf("protected: %s", got)
	}
	// A sync's bytes still to come are no reason while the card has the room.
	e, f = setup(1000, 100<<30)
	e.s.trim(context.Background(), 100, 50<<30)
	if got := there(f); got != "1111" {
		t.Errorf("reserve with room: %s", got)
	}
	// The floor of free space, and a sync's bytes on top of it: the cache gives way.
	e, f = setup(1000, cacheFloor+50)
	e.s.trim(context.Background(), 100, 0)
	if got := there(f); got != "1100" { // 50 short, and a sixteenth of the limit (62) on top: two songs
		t.Errorf("floor: %s", got)
	}
	e, f = setup(1000*mb, cacheFloor+10*mb)
	e.s.trim(context.Background(), mb, 20*mb)
	if got := there(f); got != "0000" {
		t.Errorf("sync first: %s", got)
	}
	// Off: what is kept goes at the next streamed song.
	e, f = setup(0, 1<<40)
	e.s.protect([]string{f[0]})
	e.s.trim(context.Background(), 100, 0)
	if got := there(f); got != "1000" {
		t.Errorf("off: %s", got)
	}
	// Only the cache's own files, four folders down, are ever deleted.
	e, f = setup(0, 1<<40)
	flat := filepath.Join(e.s.dir(), "queue-1.m3u")
	put(t, flat, 10, 1)
	put(t, filepath.Join(e.root, ".subsonic", "manifest.json"), 10, 1)
	put(t, filepath.Join(e.s.dir(), "acct", "note.txt"), 10, 1)
	if e.s.clearCache() || there(f) != "0000" {
		t.Errorf("clear: %s", there(f))
	}
	for _, p := range []string{flat, filepath.Join(e.root, ".subsonic", "manifest.json"), filepath.Join(e.s.dir(), "acct", "note.txt")} {
		if !exists(p) {
			t.Errorf("%s went", p)
		}
	}
}

func TestCacheMakeRoomAndReclaimable(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.cacheOn(1<<30, 150+planMargin)
	var f []string
	for i := 0; i < 3; i++ {
		f = append(f, e.kept(i, "flac", "original"))
		put(t, f[i], 100, 10-i)
	}
	e.s.protect([]string{f[2]})
	if got := e.s.reclaimable(); got != 200 {
		t.Fatalf("reclaimable %d", got)
	}
	if !e.s.makeRoom(100) || !exists(f[0]) || !exists(f[1]) { // fits
		t.Fatal("evicted with room to spare")
	}
	if !e.s.makeRoom(250) || !exists(f[0]) || exists(f[1]) || !exists(f[2]) { // 100 short: the oldest that may go
		t.Fatalf("after makeRoom: %v %v %v", exists(f[0]), exists(f[1]), exists(f[2]))
	}
	// A list started after the preview holds what the preview counted: the sync is told there is
	// no room, and nothing protected goes (review).
	e.s.protect([]string{f[0], f[2]})
	if e.s.makeRoom(350+planMargin) || !exists(f[0]) || !exists(f[2]) {
		t.Fatalf("room said to be there: %v %v", exists(f[0]), exists(f[2]))
	}
}

// A walk that deletes for long (a clear, a sync's makeRoom) holds up neither a new list, nor its
// songs' fetches, nor a stop (review).
func TestCacheWalkHoldsNoStart(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.cacheOn(1, 1<<40) // over its limit at every fetch
	put(t, e.kept(5, "flac", "original"), 100, 1)
	e.s.cmu.Lock() // a clear at work
	defer e.s.cmu.Unlock()
	done := make(chan bool)
	go func() {
		e.s.start(songs(2, "flac"), 0)
		for !exists(e.kept(1, "flac", "original")) { // no trim before its fetch
			time.Sleep(5 * time.Millisecond)
		}
		e.s.stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream waits for the walk")
	}
}

// A list that was left, or paused for long, holds only the songs around the one playing: the rest
// of it goes with a clear, in the run that learned it and after a restart (review).
func TestCacheLeftListIsNotHeld(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.cacheOn(1<<30, 1<<40)
	e.s.idle = 300 * time.Millisecond
	list := songs(8, "flac")
	fill := func() {
		for i := range list {
			put(t, e.kept(i, "flac", "original"), 10, 1)
		}
	}
	there := func() (s string) {
		for i := range list {
			s += map[bool]string{true: "1", false: "0"}[exists(e.kept(i, "flac", "original"))]
		}
		return s
	}
	fill()
	e.s.start(list, 3)
	if !e.s.clearCache() || there() != "00011111" { // it plays: all of it from the start song on
		t.Fatalf("while it plays: %s", there())
	}
	until(t, "the run's end", func() bool { return !e.s.active() })
	if !e.s.clearCache() || there() != "00011100" {
		t.Fatalf("left: %s", there())
	}
	// The app started again: .playing still names the song, as the firmware leaves it.
	restart := func(ago time.Duration) *streamer {
		fill()
		e.setPlaying(t, e.kept(3, "flac", "original"))
		at := time.Now().Add(-ago)
		os.Chtimes(e.playing, at, at)
		s := newStreamer(e.root, e.play, e.playing, fw.Caps{Playlist: true}, e.src, e.s.manifest, e.net, 5*time.Millisecond)
		s.cacheCap, s.acct, s.free, s.reserve = e.s.cacheCap, e.s.acct, e.s.free, e.s.reserve
		s.cleanLeft()
		return s
	}
	if s := restart(time.Minute); !s.clearCache() || there() != "00011111" {
		t.Fatalf("restart while it plays: %s", there())
	}
	if s := restart(3 * time.Hour); !s.clearCache() || there() != "00011100" {
		t.Fatalf("restart long after: %s", there())
	}
}

// A song kept is stamped when it is fetched and when it plays, by the app's clock, and never back.
func TestCachePlayedStampsTheFile(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.cacheOn(1<<30, 1<<40)
	var hours atomic.Int64
	at := func() time.Time { return time.Date(2030, 1, 1, int(hours.Load()), 0, 0, 0, time.UTC) }
	e.s.clock = at // its own clock, which may wait for the server's time: plays' never does (review)
	e.s.plays.clock = func() time.Time { return time.Time{} }
	e.s.start(songs(2, "flac"), 0)
	p := e.kept(1, "flac", "original")
	mtime := func() time.Time { st, _ := os.Stat(p); return st.ModTime() }
	until(t, "fetched and stamped", func() bool { return exists(p) && mtime().Equal(at()) })
	hours.Store(1)
	e.setPlaying(t, p)
	until(t, "stamped at its start", func() bool { return mtime().Equal(at()) })
}

// The app started again while its queue plays: the queue's songs from the one playing on are not
// evicted, cleared or swept, and its queue file stays.
func TestCacheRestartKeepsPlayingQueue(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.cacheOn(1<<30, 1<<40)
	e.s.start(songs(3, "flac"), 0)
	until(t, "fetched", func() bool { return exists(e.kept(2, "flac", "original")) })
	e.s.stop()
	e.setPlaying(t, e.kept(1, "flac", "original"))
	other := filepath.Join(e.s.dir(), "gone", "original", "0", "X [x].flac")
	put(t, other, 10, 1)
	q, _ := e.queue(t)

	s := newStreamer(e.root, e.play, e.playing, fw.Caps{Playlist: true}, e.src, e.s.manifest, e.net, 5*time.Millisecond)
	s.cacheCap, s.acct, s.free, s.reserve = e.s.cacheCap, e.s.acct, e.s.free, e.s.reserve
	s.cleanLeft()
	s.sweep("acct")
	if !exists(q) || exists(other) {
		t.Fatalf("queue kept %v, the other account's song kept %v", exists(q), exists(other))
	}
	if kept := s.clearCache(); !kept {
		t.Error("clear says nothing stayed")
	}
	s.sweep("")
	for i := 0; i < 3; i++ {
		if !exists(e.kept(i, "flac", "original")) {
			t.Errorf("song %d of the playing queue went", i)
		}
	}
	// .playing names something else: the queue was left, and nothing is held.
	os.WriteFile(e.playing, []byte("/elsewhere.flac\n"), 0o644)
	s.cleanLeft()
	if exists(q) || s.clearCache() || exists(e.kept(1, "flac", "original")) {
		t.Error("a queue that was left is still held")
	}
}

func TestSettingsStreamCache(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "playlist", true)
	want := []string{"Stream Cache: 4 GB", "Stream Cache: 8 GB", "Stream Cache: Off", "Stream Cache: 512 MB", "Stream Cache: 1 GB", "Stream Cache: 2 GB"}
	if _, ok := find(d.do("settings"), "Stream Cache: 2 GB"); !ok {
		t.Fatalf("rows %q", titles(d.do("settings")))
	}
	for _, w := range want {
		if r := d.do("cachesz"); r.Message != w {
			t.Fatalf("toast %q, want %q", r.Message, w)
		}
		if row, ok := find(d.do("settings"), w); !ok || row.Kind != "action" || row.URI != "cachesz" {
			t.Fatalf("rows %q", titles(d.do("settings")))
		}
	}
	if d.settings().cacheBytes() != 2<<30 {
		t.Errorf("limit %d", d.settings().cacheBytes())
	}
	// Without the firmware's playlist word nothing is kept: no rows.
	old := newTestDaemon(t, &nav{}, "", true)
	if got := strings.Join(titles(old.do("settings")), "|"); strings.Contains(got, "Cache") {
		t.Errorf("rows %q", got)
	}
}

// cacheFile is a song in the signed-in account's cache.
func (d *tdaemon) cacheFile(t *testing.T, id string, n int) string {
	t.Helper()
	if d.client() == nil {
		t.Fatal("signed out")
	}
	p := d.stream.cachePath(d.stream.cacheBase(), subsonic.Song{ID: id, Title: "Song 0"}, "flac", "original")
	put(t, p, n, 1)
	return p
}

func TestClearCacheNeedsTwoPresses(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "playlist", true)
	d.bg.Wait() // the start-up count
	if row, _ := find(d.do("settings"), "Clear Cache"); row.Subtitle != "Empty" || row.URI != "cacheclr" {
		t.Fatalf("row %+v", row)
	}
	a, b := d.cacheFile(t, "a", 3<<20), d.cacheFile(t, "b", 2<<20)
	d.stream.reclaimable() // counts
	if row, _ := find(d.do("settings"), "Clear Cache"); row.Subtitle != "5 MB used" {
		t.Fatalf("row %+v", row)
	}
	if r := d.do("cacheclr"); r.Message != "Press again to clear the cache" || !exists(a) {
		t.Fatalf("first press: %q", r.Message)
	}
	if row, _ := find(d.do("settings"), "Clear Cache"); row.Subtitle != "Press again to clear it" {
		t.Fatalf("armed row %+v", row)
	}
	d.do("report") // another setting pressed: not armed any more
	if r := d.do("cacheclr"); r.Message != "Press again to clear the cache" || !exists(a) {
		t.Fatalf("press after another setting: %q", r.Message)
	}
	d.stream.protect([]string{b})
	if r := d.do("cacheclr"); r.Message != "Cache cleared · the list playing is kept" || exists(a) || !exists(b) {
		t.Fatalf("second press: %q, a %v b %v", r.Message, exists(a), exists(b))
	}
	d.stream.protect(nil)
	d.do("cacheclr")
	if r := d.do("cacheclr"); r.Message != "Cache cleared" || exists(b) {
		t.Fatalf("cleared: %q", r.Message)
	}
	if row, _ := find(d.do("settings"), "Clear Cache"); row.Subtitle != "Empty" {
		t.Fatalf("row %+v", row)
	}
}

func TestSignOutClearsCache(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "playlist", true)
	d.bg.Wait()
	a, b := d.cacheFile(t, "a", 10), d.cacheFile(t, "b", 10)
	d.stream.protect([]string{b}) // the Q2 may play on
	d.signOut()
	d.bg.Wait()
	if exists(a) || !exists(b) {
		t.Fatalf("after Sign Out: a %v b %v", exists(a), exists(b))
	}
}

// At start-up another account's songs go, and with nobody signed in all of them.
func TestStartSweepsOtherAccounts(t *testing.T) {
	card := t.TempDir()
	dir := filepath.Join(card, ".subsonic")
	os.MkdirAll(dir, 0o755)
	creds.Save(dir, subsonic.Creds{Server: "https://music.example", User: "u", Password: "pw"})
	mine := filepath.Join(dir, "stream", acctDir("https://music.example", "u"), "original", "0", "A [a].flac")
	other := filepath.Join(dir, "stream", acctDir("https://music.example", "v"), "original", "0", "A [a].flac")
	put(t, mine, 10, 1)
	put(t, other, 10, 1)
	d := newDaemon(dir, card, t.TempDir())
	d.bg.Wait()
	if !exists(mine) || exists(other) || d.stream.used.Load() != 10 {
		t.Fatalf("mine %v, the other's %v, used %d", exists(mine), exists(other), d.stream.used.Load())
	}
	creds.Remove(dir)
	d = newDaemon(dir, card, t.TempDir())
	d.bg.Wait()
	if exists(mine) {
		t.Fatal("kept with nobody signed in")
	}
}

func TestGotoCachedSong(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2}
	d := newTestDaemon(t, n, "playlist", true)
	d.bg.Wait()
	p := d.cacheFile(t, "al1.0", 1)
	r := d.do("goto", "album", p)
	if got := strings.Join(titles(r), "|"); got != "Add to Sync|Play|Song 0|Song 1" || r.Status != "" {
		t.Fatalf("rows %q, status %q %q", got, r.Status, r.Message)
	}
	other := strings.Replace(p, "/"+d.acct()+"/", "/00000000/", 1)
	if r := d.do("goto", "album", other); r.Message != "Not a Subsonic song" {
		t.Fatalf("another account's song: %q %q", r.Status, r.Message)
	}
}

// The preview counts what the cache can give back, and says when the sync takes it.
func TestPreviewTakesFromCache(t *testing.T) {
	p := &plan.Plan{Add: []plan.Want{{Bytes: 3 << 30}}, NeedBytes: 3 << 30, FreeBytes: 4 << 30, Fits: true}
	got := strings.Join(titles(previewRows(p, 2<<30)), "|")
	for _, w := range []string{"Needs 3.2 GB · 2.1 GB free", "Takes 1.1 GB from the Stream Cache", "Start Sync"} {
		if !strings.Contains(got, w) {
			t.Errorf("no %q in %q", w, got)
		}
	}
	if got := strings.Join(titles(previewRows(p, 0)), "|"); strings.Contains(got, "Stream Cache") || !strings.Contains(got, "4.3 GB free") {
		t.Errorf("with room: %q", got)
	}

	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "playlist", true)
	d.bg.Wait()
	d.cacheFile(t, "a", 64<<20)
	d.free = func(string) int64 { return 1 << 20 }
	d.do("toggle", "album:al0")
	r := d.preview(t)
	if _, ok := find(r, "Start Sync"); !ok {
		t.Fatalf("rows %q", titles(r))
	}
	if _, ok := find(r, "Takes "); !ok {
		t.Fatalf("rows %q", titles(r))
	}
}
