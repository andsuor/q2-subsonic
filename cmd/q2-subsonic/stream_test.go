package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

type fakeSrc struct {
	mu    sync.Mutex
	urls  []string
	onGet func()
	fail  func(u string) bool
	err   func(u string) error              // optional: the error Get answers u with (nil: none)
	body  func(u string) (io.Reader, int64) // optional: the answer's body and its Content-Length (-1: none sent)
}

func (f *fakeSrc) MediaURL(id, q string) string { return "u:" + id + ":" + q }
func (f *fakeSrc) PlayURL(id, q string) string  { return "p:" + id + ":" + q }
func (f *fakeSrc) Get(_ context.Context, u string, _ int64) (*http.Response, error) {
	f.mu.Lock()
	f.urls = append(f.urls, u)
	f.mu.Unlock()
	if f.onGet != nil {
		f.onGet()
	}
	if f.fail != nil && f.fail(u) {
		return nil, fmt.Errorf("boom")
	}
	if f.err != nil {
		if err := f.err(u); err != nil {
			return nil, err
		}
	}
	if f.body != nil {
		b, n := f.body(u)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(b), ContentLength: n}, nil
	}
	b := "data-" + u
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(b)), ContentLength: int64(len(b))}, nil
}

func (f *fakeSrc) tries(id string) int {
	n := 0
	for _, u := range f.got() {
		if strings.Contains(u, ":"+id+":") {
			n++
		}
	}
	return n
}

func (f *fakeSrc) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...)
}

type env struct {
	root, play, playing string
	src                 *fakeSrc
	man                 *library.Manifest
	net                 *syncer.NetSlot
	s                   *streamer
}

func newEnv(t *testing.T, caps fw.Caps) *env {
	t.Helper()
	e := &env{root: t.TempDir(), src: &fakeSrc{}, man: &library.Manifest{Songs: map[string]*library.Entry{}}, net: syncer.NewNetSlot()}
	tmp := t.TempDir()
	e.play, e.playing = tmp+"/play", tmp+"/playing"
	e.s = newStreamer(e.root, e.play, e.playing, caps, e.src, func() *library.Manifest { return e.man }, e.net, 5*time.Millisecond)
	e.s.gap = 0
	t.Cleanup(e.s.stop)
	return e
}

func songs(n int, suffix string) []subsonic.Song {
	var out []subsonic.Song
	for i := 0; i < n; i++ {
		out = append(out, subsonic.Song{ID: fmt.Sprint("s", i), Title: fmt.Sprint("T", i), Suffix: suffix})
	}
	return out
}

func (e *env) stream(i int, ext string) string {
	return fmt.Sprintf("%s/.subsonic/stream/%04d - T%d [s%d].%s", e.root, i, i, i, ext)
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout: " + what)
}

func (e *env) queue(t *testing.T) (path, body string) {
	t.Helper()
	m, _ := filepath.Glob(e.root + "/.subsonic/stream/queue-*.m3u")
	if len(m) != 1 {
		t.Fatalf("queue files: %v", m)
	}
	b, _ := os.ReadFile(m[0])
	return m[0], string(b)
}

func TestStreamQueueWrittenOnce(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.start(songs(4, "flac"), 0)
	until(t, "play", func() bool { return exists(e.play) })
	q1, body1 := e.queue(t)
	st1, _ := os.Stat(q1)
	pl1, _ := os.ReadFile(e.play)
	until(t, "fetched", func() bool { return exists(e.stream(2, "flac")) })
	time.Sleep(30 * time.Millisecond)
	if st2, _ := os.Stat(q1); !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatal("queue file rewritten")
	}
	if b, _ := os.ReadFile(q1); string(b) != body1 {
		t.Fatal("queue content changed")
	}
	e.s.start(songs(3, "flac"), 1)
	q2, _ := e.queue(t)
	pl2, _ := os.ReadFile(e.play)
	if q2 == q1 || string(pl1) == string(pl2) || exists(q1) {
		t.Fatalf("second start: %s %s %q %q", q1, q2, pl1, pl2)
	}
	if want := strings.Fields(string(pl2)); len(want) != 2 || want[1] != q2 {
		t.Fatalf("play = %q", pl2)
	}
}

func TestStreamQueueUsesSyncedPaths(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	synced := e.root + "/Subsonic/A/b.flac"
	os.MkdirAll(filepath.Dir(synced), 0o755)
	os.WriteFile(synced, []byte("x"), 0o644)
	e.man.Songs["s1"] = &library.Entry{ID: "s1", Path: synced}
	e.s.start(songs(3, "flac"), 0)
	until(t, "play", func() bool { return exists(e.play) })
	_, body := e.queue(t)
	want := "#EXTM3U\n" + e.stream(0, "flac") + "\n" + synced + "\n" + e.stream(2, "flac") + "\n"
	if body != want {
		t.Fatalf("got %q want %q", body, want)
	}
	until(t, "s2", func() bool { return exists(e.stream(2, "flac")) })
	if exists(e.stream(1, "flac")) {
		t.Fatal("synced song fetched")
	}
}

func TestStreamFallbackTranscodesOthers(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	ss := songs(2, "m4a")
	ss[1].Suffix = "flac"
	e.s.start(ss, 0)
	until(t, "files", func() bool { return exists(e.stream(0, "mp3")) && exists(e.stream(1, "flac")) })
	until(t, "play", func() bool { return exists(e.play) })
	e.src.mu.Lock()
	defer e.src.mu.Unlock()
	if e.src.urls[0] != "p:s0:mp3-320" || e.src.urls[1] != "p:s1:original" {
		t.Fatal(e.src.urls)
	}
	if b, _ := os.ReadFile(e.play); !strings.HasSuffix(string(b), " "+e.root+"/.subsonic/stream") {
		t.Fatalf("play = %q", b)
	}
}

func TestStreamPrefetchTwoAhead(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	e.s.start(songs(8, "flac"), 0)
	until(t, "0-2", func() bool { return exists(e.stream(2, "flac")) })
	os.WriteFile(e.playing, []byte(e.stream(3, "flac")+"\n"), 0o644)
	until(t, "4,5", func() bool { return exists(e.stream(4, "flac")) && exists(e.stream(5, "flac")) })
	time.Sleep(40 * time.Millisecond)
	if exists(e.stream(6, "flac")) {
		t.Fatal("fetched 3 ahead")
	}
}

func TestStreamTrimsPlayed(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	e.s.start(songs(8, "flac"), 0)
	until(t, "0-2", func() bool { return exists(e.stream(2, "flac")) })
	synced := e.root + "/Subsonic/keep.flac"
	os.MkdirAll(filepath.Dir(synced), 0o755)
	os.WriteFile(synced, []byte("x"), 0o644)
	os.WriteFile(e.playing, []byte(e.stream(5, "flac")), 0o644)
	until(t, "trim", func() bool {
		return !exists(e.stream(0, "flac")) && !exists(e.stream(2, "flac")) && exists(e.stream(7, "flac"))
	})
	if !exists(synced) {
		t.Fatal("synced file deleted")
	}
}

func TestStreamPriority(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	e.src.onGet = func() { note("stream") }
	ctx := context.Background()
	e.net.Acquire(ctx, false) // the syncer's current file
	go func() { e.net.Acquire(ctx, false); note("sync"); e.net.Release() }()
	time.Sleep(20 * time.Millisecond)
	e.s.start(songs(1, "flac"), 0)
	time.Sleep(20 * time.Millisecond)
	e.net.Release()
	until(t, "both", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 2 })
	if order[0] != "stream" {
		t.Fatal(order)
	}
}

func TestStartIgnoresStalePlaying(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	os.WriteFile(e.playing, []byte(e.stream(5, "flac")), 0o644) // left by the previous list
	e.s.start(songs(8, "flac"), 0)
	until(t, "song 0", func() bool { return exists(e.stream(0, "flac")) })
	until(t, "play", func() bool { return exists(e.play) })
	time.Sleep(30 * time.Millisecond)
	if exists(e.stream(6, "flac")) {
		t.Fatal("stale .playing used")
	}
}

func TestFetchRetriesThenErr(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	e.s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	e.src.fail = func(u string) bool { return strings.Contains(u, "s1") }
	e.s.start(songs(4, "flac"), 0)
	until(t, "err", func() bool { return e.s.Err() != nil })
	if e.s.Err().Error() != "Couldn't stream T1" {
		t.Fatal(e.s.Err())
	}
	until(t, "stopped", func() bool { return !e.s.active() })
	n := 0
	for _, u := range e.src.urls {
		if strings.Contains(u, "s1") {
			n++
		}
	}
	if n != 3 || exists(e.stream(2, "flac")) {
		t.Fatalf("tries=%d", n)
	}
}

func TestLastSongFailureRetries(t *testing.T) {
	e := newEnv(t, fw.Caps{})
	e.s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	var mu sync.Mutex
	tries := 0
	e.src.fail = func(string) bool { mu.Lock(); defer mu.Unlock(); tries++; return tries == 1 }
	e.s.start(songs(1, "flac"), 0)
	until(t, "fetched", func() bool { return exists(e.stream(0, "flac")) })
	if e.s.Err() != nil {
		t.Fatal(e.s.Err())
	}
}

func TestPlaylistModeOriginalOnlyForPlayable(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	ss := songs(3, "M4A")
	ss[1].Suffix, ss[2].Suffix = "wv", "opus" // WavPack the Q2 can't play; Opus it can (R40)
	e.s.start(ss, 0)
	until(t, "files", func() bool {
		return exists(e.stream(0, "m4a")) && exists(e.stream(1, "mp3")) && exists(e.stream(2, "opus"))
	})
	if u := e.src.got(); u[0] != "p:s0:original" || u[1] != "p:s1:mp3-320" || u[2] != "p:s2:original" {
		t.Fatal(u)
	}
}

// Playback streams an original as it is through PlayURL (stream?format=raw), which servers allow
// with downloads turned off; MediaURL's download is for sync (M27, R37).
func TestStreamUsesPlayURL(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.start(songs(1, "flac"), 0)
	until(t, "fetched", func() bool { return exists(e.stream(0, "flac")) })
	if u := e.src.got(); len(u) != 1 || u[0] != "p:s0:original" {
		t.Fatal(u)
	}
}

// A synced song whose file is gone, or that a sync is about to move or remove, is streamed, not
// queued at a path that never comes (M24). One with an ellipsis in its path plays from the card:
// the firmware skips only a real ".." segment (I19).
func TestStreamSyncedOnlyWhenThere(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	here, gone, moving, dotted := e.root+"/Subsonic/A/0.flac", e.root+"/Subsonic/A/1.flac", e.root+"/Subsonic/A/2.flac",
		e.root+"/Subsonic/...Like Clockwork/3.flac"
	for _, f := range []string{here, moving, dotted} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("x"), 0o644)
	}
	for i, f := range []string{here, gone, moving, dotted} {
		id := fmt.Sprint("s", i)
		e.man.Songs[id] = &library.Entry{ID: id, Path: f}
	}
	e.s.moving = func(id string) bool { return id == "s2" }
	e.s.start(songs(4, "flac"), 0)
	until(t, "play", func() bool { return exists(e.play) })
	_, body := e.queue(t)
	want := "#EXTM3U\n" + here + "\n" + e.stream(1, "flac") + "\n" + e.stream(2, "flac") + "\n" + dotted + "\n"
	if body != want {
		t.Fatalf("got %q want %q", body, want)
	}
}

// Stream names never hold "..", which the firmware skips (I19).
func TestStreamNameWithDots(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	ss := songs(1, "flac")
	ss[0].Title = "Oops!... I Did It Again"
	e.s.start(ss, 0)
	until(t, "play", func() bool { return exists(e.play) })
	if _, body := e.queue(t); strings.Contains(body, "..") {
		t.Fatal(body)
	}
}

// A synced song played twice in a list: the second time is found after the first, so the songs
// after it are fetched (M25).
func TestStreamDuplicateSyncedSongs(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	a, b := e.root+"/Subsonic/A/a.flac", e.root+"/Subsonic/A/b.flac"
	for _, f := range []string{a, b} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("x"), 0o644)
	}
	ss := []subsonic.Song{{ID: "A"}, {ID: "B"}, {ID: "s2", Title: "T2"}, {ID: "s3", Title: "T3"}, {ID: "s4", Title: "T4"},
		{ID: "A"}, {ID: "B"}, {ID: "s7", Title: "T7"}}
	for i := range ss {
		ss[i].Suffix = "flac"
	}
	e.man.Songs["A"] = &library.Entry{ID: "A", Path: a}
	e.man.Songs["B"] = &library.Entry{ID: "B", Path: b}
	e.s.start(ss, 0)
	until(t, "x2", func() bool { return exists(e.stream(2, "flac")) })
	for _, p := range []string{a, b, e.stream(2, "flac"), e.stream(3, "flac"), e.stream(4, "flac"), a, b} {
		os.WriteFile(e.playing, []byte(p+"\n"), 0o644)
		time.Sleep(30 * time.Millisecond)
	}
	until(t, "y7", func() bool { return exists(e.stream(7, "flac")) })
}

// Once the songs around the one playing are on the card, the stream moves no data: hold is off,
// so the Q2 can sleep. With no new song for the idle time the stream ends, but the songs the
// firmware has queued around the one playing stay: they still play (I14, M26).
func TestStreamIdleEnds(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.idle = 300 * time.Millisecond
	e.s.start(songs(12, "flac"), 0)
	until(t, "window", func() bool { return exists(e.stream(2, "flac")) })
	until(t, "not busy", func() bool { return !e.s.busy() })
	if !e.s.active() {
		t.Fatal("ended before the idle time")
	}
	os.WriteFile(e.playing, []byte(e.stream(1, "flac")+"\n"), 0o644)
	until(t, "busy for song 3", func() bool { return exists(e.stream(3, "flac")) })
	until(t, "ended", func() bool { return !e.s.active() })
	for i := 1; i <= 3; i++ {
		if !exists(e.stream(i, "flac")) {
			t.Fatalf("song %d deleted: the firmware has it queued", i)
		}
	}
	if e.s.busy() {
		t.Fatal("busy after the end")
	}
}

// The idle time counts from the end of the song playing: a song longer than it (a live set, a
// symphony in one file) doesn't end the list (I14).
func TestStreamIdleAfterLongSong(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.idle = 100 * time.Millisecond
	ss := songs(6, "flac")
	ss[0].Duration = 1 // second
	e.s.start(ss, 0)
	until(t, "window", func() bool { return exists(e.stream(2, "flac")) })
	time.Sleep(600 * time.Millisecond)
	if !e.s.active() {
		t.Fatal("ended while a song longer than the idle time plays")
	}
	until(t, "ended", func() bool { return !e.s.active() })
}

// A list played to its last song keeps the songs behind it in the window, which Prev reaches;
// older ones go (M26).
func TestStreamEndKeepsWindow(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.start(songs(6, "flac"), 0)
	until(t, "window", func() bool { return exists(e.stream(2, "flac")) })
	os.WriteFile(e.playing, []byte(e.stream(3, "flac")+"\n"), 0o644)
	until(t, "4, 5", func() bool { return exists(e.stream(4, "flac")) && exists(e.stream(5, "flac")) })
	os.WriteFile(e.playing, []byte(e.stream(5, "flac")+"\n"), 0o644)
	until(t, "ended", func() bool { return !e.s.active() })
	if exists(e.stream(0, "flac")) || exists(e.stream(2, "flac")) {
		t.Fatal("songs long played left")
	}
	for i := 3; i <= 5; i++ {
		if !exists(e.stream(i, "flac")) {
			t.Fatalf("song %d deleted", i)
		}
	}
}

// A body that runs past its song's size (no Content-Length) is cut, never left to fill the card;
// one shorter than its Content-Length, or empty, is no song (M9, I8).
func TestStreamBodyBounds(t *testing.T) {
	for name, body := range map[string]func(string) (io.Reader, int64){
		"endless": func(string) (io.Reader, int64) { return io.LimitReader(zeros{}, 40<<20), -1 },
		"short":   func(string) (io.Reader, int64) { return strings.NewReader("half"), 1000 },
		"empty":   func(string) (io.Reader, int64) { return strings.NewReader(""), -1 },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, fw.Caps{Playlist: true})
			e.s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
			e.src.body = body
			ss := songs(1, "flac")
			ss[0].Size = 1000
			e.s.start(ss, 0)
			until(t, "err", func() bool { return e.s.Err() != nil })
			if exists(e.stream(0, "flac")) {
				t.Fatal("kept as the song")
			}
		})
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// errFor answers the songs named with err; the others play.
func errFor(err error, ids ...string) func(string) error {
	return func(u string) error {
		for _, id := range ids {
			if strings.Contains(u, ":"+id+":") {
				return err
			}
		}
		return nil
	}
}

// A song the server can't send (404) is dropped from the queue, which is rewritten aside and
// renamed into place with every line before it as it was, and the songs after it play on.
func TestStreamSkipsSongItsServerLacks(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	e.src.err = errFor(&subsonic.APIError{Op: "stream", HTTP: 404, Msg: "Not Found"}, "s1")
	ss := songs(4, "flac")
	orig := slices.Clone(ss)
	e.s.start(ss, 0)
	q, before := e.queue(t) // written whole before start returns
	st1, _ := os.Stat(q)
	until(t, "songs 3, 4", func() bool { return exists(e.stream(2, "flac")) && exists(e.stream(3, "flac")) })
	_, after := e.queue(t)
	head := "#EXTM3U\n" + e.stream(0, "flac") + "\n"
	if !strings.HasPrefix(before, head+e.stream(1, "flac")+"\n") || after != head+e.stream(2, "flac")+"\n"+e.stream(3, "flac")+"\n" {
		t.Fatalf("queue %q, was %q", after, before)
	}
	if st2, _ := os.Stat(q); os.SameFile(st1, st2) {
		t.Fatal("queue written in place, not aside and renamed")
	}
	if m, _ := filepath.Glob(e.root + "/.subsonic/stream/*.tmp"); len(m) != 0 {
		t.Fatal(m)
	}
	if err := e.s.Err(); err == nil || err.Error() != "Skipped T1: Not Found" {
		t.Fatalf("err %v", err)
	}
	if !e.s.active() {
		t.Fatal("run ended")
	}
	if !reflect.DeepEqual(ss, orig) {
		t.Fatalf("the caller's list changed: %v", ss)
	}
	if n := e.src.tries("s1"); n != 3 {
		t.Fatalf("a bare 404 tried %d times", n) // a proxy or a deploy can answer one for a while
	}
}

// Losing the server, a proxy's answer that it is down, or a refusal for the account fails every
// song alike: the run ends at the song, as before, and the queue is left as it was.
func TestStreamConnectionLossEnds(t *testing.T) {
	for name, err := range map[string]error{
		"offline":   &subsonic.APIError{Op: "stream", Msg: "connection refused"},
		"proxy 502": &subsonic.APIError{Op: "stream", HTTP: 502, Msg: "Bad Gateway"},
		"429":       &subsonic.APIError{Op: "stream", HTTP: 429, Msg: "Too Many Requests"},
		"code 50":   &subsonic.APIError{Op: "stream", HTTP: 200, Code: 50, Msg: "not authorized"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, fw.Caps{Playlist: true})
			e.s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
			e.src.err = errFor(err, "s1")
			e.s.start(songs(4, "flac"), 0)
			_, before := e.queue(t)
			until(t, "ended", func() bool { return !e.s.active() })
			if _, after := e.queue(t); after != before {
				t.Fatalf("queue %q, was %q", after, before)
			}
			if e.s.Err() == nil || e.s.Err().Error() != "Couldn't stream T1" || exists(e.stream(2, "flac")) {
				t.Fatalf("err %v", e.s.Err())
			}
		})
	}
}

// Two songs skipped is two songs' own; a third in a row means it isn't: the run ends there, the
// queue still waiting at that song.
func TestStreamThirdFailureInARowEnds(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	e.src.err = errFor(&subsonic.APIError{Op: "stream", HTTP: 404, Msg: "Not Found"}, "s1", "s2", "s3")
	e.s.start(songs(6, "flac"), 0)
	until(t, "ended", func() bool { return !e.s.active() })
	_, body := e.queue(t)
	want := "#EXTM3U\n" + e.stream(0, "flac") + "\n" + e.stream(3, "flac") + "\n" + e.stream(4, "flac") + "\n" +
		e.stream(5, "flac") + "\n"
	if body != want {
		t.Fatalf("queue %q, want %q", body, want)
	}
	if e.s.Err() == nil || e.s.Err().Error() != "Couldn't stream T3" || exists(e.stream(4, "flac")) {
		t.Fatalf("err %v", e.s.Err())
	}
}

// The server's own word that a song is gone (Subsonic 70, HTTP 410) is skipped at its first try,
// and the next song is fetched at once: the firmware waits for it.
func TestStreamSkipsGoneSongAtOnce(t *testing.T) {
	for name, err := range map[string]error{
		"code 70": &subsonic.APIError{Op: "stream", HTTP: 200, Code: 70, Msg: "Song not found"},
		"410":     &subsonic.APIError{Op: "stream", HTTP: 410, Msg: "Gone"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, fw.Caps{Playlist: true})
			e.s.backoff = []time.Duration{time.Hour, time.Hour, time.Hour}
			e.s.poll = time.Hour
			e.src.err = errFor(err, "s1")
			e.s.start(songs(4, "flac"), 0)
			until(t, "songs 3, 4", func() bool { return exists(e.stream(2, "flac")) && exists(e.stream(3, "flac")) })
			if n := e.src.tries("s1"); n != 1 {
				t.Fatalf("tried %d times", n)
			}
			if _, body := e.queue(t); body != "#EXTM3U\n"+e.stream(0, "flac")+"\n"+e.stream(2, "flac")+"\n"+e.stream(3, "flac")+"\n" {
				t.Fatalf("queue %q", body)
			}
		})
	}
}

// A song the firmware may have passed is never dropped: one that landed in this run, was trimmed
// behind the one playing and is fetched again after Prev ends the run as before, the queue as it was.
func TestStreamLandedSongNeverSkipped(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	var dead atomic.Bool
	e.src.err = func(u string) error {
		if dead.Load() && strings.Contains(u, ":s0:") {
			return &subsonic.APIError{Op: "stream", HTTP: 404, Msg: "Not Found"}
		}
		return nil
	}
	e.s.start(songs(8, "flac"), 0)
	_, before := e.queue(t)
	until(t, "0-2", func() bool { return exists(e.stream(2, "flac")) })
	os.WriteFile(e.playing, []byte(e.stream(2, "flac")), 0o644)
	until(t, "3, 4", func() bool { return exists(e.stream(4, "flac")) })
	os.WriteFile(e.playing, []byte(e.stream(4, "flac")), 0o644)
	until(t, "0, 1 trimmed", func() bool { return !exists(e.stream(0, "flac")) && !exists(e.stream(1, "flac")) })
	dead.Store(true)
	os.WriteFile(e.playing, []byte(e.stream(0, "flac")), 0o644)
	until(t, "ended", func() bool { return !e.s.active() })
	if _, after := e.queue(t); after != before {
		t.Fatalf("queue %q, was %q", after, before)
	}
	if e.s.Err() == nil || e.s.Err().Error() != "Couldn't stream T0" {
		t.Fatalf("err %v", e.s.Err())
	}
}

// A song that can't be written to the card is the card's failure, not the song's: the run ends as
// before, the queue as it was.
func TestStreamCardWriteErrorEnds(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	e.src.err = func(u string) error {
		if strings.Contains(u, ":s0:") { // song 1's .part becomes a folder that won't go
			os.MkdirAll(e.stream(1, "flac")+".part/x", 0o755)
		}
		return nil
	}
	e.s.start(songs(4, "flac"), 0)
	_, before := e.queue(t)
	until(t, "ended", func() bool { return !e.s.active() })
	if _, after := e.queue(t); after != before {
		t.Fatalf("queue %q, was %q", after, before)
	}
	if e.s.Err() == nil || e.s.Err().Error() != "Couldn't stream T1" {
		t.Fatalf("err %v", e.s.Err())
	}
}

// The queue's first line, the list's first song or not, may go too.
func TestStreamSkipFirstLine(t *testing.T) {
	for _, c := range []struct {
		from int
		dead string
	}{{0, "s0"}, {1, "s1"}, {1, "s2"}} {
		t.Run(fmt.Sprint(c.from, c.dead), func(t *testing.T) {
			e := newEnv(t, fw.Caps{Playlist: true})
			e.s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
			e.src.err = errFor(&subsonic.APIError{Op: "stream", HTTP: 404, Msg: "Not Found"}, c.dead)
			e.s.start(songs(4, "flac"), c.from)
			want := "#EXTM3U\n"
			for i := c.from; i < 4; i++ {
				if fmt.Sprint("s", i) != c.dead {
					want += e.stream(i, "flac") + "\n"
					until(t, fmt.Sprint("song ", i), func() bool { return exists(e.stream(i, "flac")) })
				}
			}
			if _, body := e.queue(t); body != want {
				t.Fatalf("queue %q, want %q", body, want)
			}
		})
	}
}

// Two rewrites of the queue are at least the gap apart, counted from its first write: the firmware
// notices a change only by the file's mtime, which FAT32 keeps in 2 s.
func TestStreamQueueRewritesApart(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.gap = 100 * time.Millisecond
	e.src.err = errFor(&subsonic.APIError{Op: "stream", HTTP: 200, Code: 70, Msg: "Song not found"}, "s1", "s2")
	e.s.start(songs(5, "flac"), 0)
	q, _ := e.queue(t)
	st1, _ := os.Stat(q)
	until(t, "songs 4, 5", func() bool { return exists(e.stream(3, "flac")) && exists(e.stream(4, "flac")) })
	st2, _ := os.Stat(q)
	if d := st2.ModTime().Sub(st1.ModTime()); d < 150*time.Millisecond {
		t.Fatalf("two rewrites within %v of the first write", d)
	}
	if _, body := e.queue(t); body != "#EXTM3U\n"+e.stream(0, "flac")+"\n"+e.stream(3, "flac")+"\n"+e.stream(4, "flac")+"\n" {
		t.Fatalf("queue %q", body)
	}
}
