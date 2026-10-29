package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
}

func (f *fakeSrc) MediaURL(id, q string) string { return "u:" + id + ":" + q }
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
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data-" + u))}, nil
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
	if e.src.urls[0] != "u:s0:mp3-320" || e.src.urls[1] != "u:s1:original" {
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
	ss := songs(2, "M4A")
	ss[1].Suffix = "opus"
	e.s.start(ss, 0)
	until(t, "files", func() bool { return exists(e.stream(0, "m4a")) && exists(e.stream(1, "mp3")) })
	e.src.mu.Lock()
	defer e.src.mu.Unlock()
	if e.src.urls[0] != "u:s0:original" || e.src.urls[1] != "u:s1:mp3-320" {
		t.Fatal(e.src.urls)
	}
}
