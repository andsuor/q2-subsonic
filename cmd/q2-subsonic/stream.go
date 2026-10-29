package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

const streamWindow = 2 // songs fetched ahead of, and kept behind, the one playing

type streamSource interface {
	PlayURL(id, quality string) string
	Get(ctx context.Context, u string, from int64) (*http.Response, error)
}

// streamer plays a list by fetching songs a couple ahead of .playing into the hidden stream
// folder and telling the firmware what to play through .play. Streamed files never go in the manifest.
type streamer struct {
	root, playFile, playingFile string // root is the card (/mnt/mmc)
	caps                        fw.Caps
	src                         streamSource
	manifest                    func() *library.Manifest
	net                         *syncer.NetSlot
	poll                        time.Duration
	backoff                     []time.Duration      // wait after each failed try of a song; one try per entry
	log                         *applog.Log          // optional
	quality                     func() string        // optional Streaming Quality; mp3-320/mp3-128 transcode every streamed song
	moving                      func(id string) bool // optional: a sync running or paused will move or remove this song
	// idle ends a stream when no new song has played for this long and nothing is left to fetch: the
	// firmware leaves .playing alone once it plays anything else, so this is also how a stream learns
	// that it was left. Default idleAfter.
	idle time.Duration

	errMu sync.Mutex
	err   *streamErr

	xfer atomic.Bool // a song of the window is still to fetch (busy)

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func newStreamer(root, play, playing string, caps fw.Caps, src streamSource, man func() *library.Manifest,
	net *syncer.NetSlot, poll time.Duration) *streamer {
	return &streamer{root: root, playFile: play, playingFile: playing, caps: caps, src: src, manifest: man, net: net, poll: poll,
		backoff: []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second}}
}

// streamErr is a song that failed all its tries: what the root shows, and its Details.
type streamErr struct {
	title, id string
	at        time.Time
	err       error // the last try's
}

func (e *streamErr) Error() string { return "Couldn't stream " + e.title }
func (e *streamErr) Unwrap() error { return e.err }

// Err is set when a song failed all its tries; prefetching stops at that song. A new list or a
// stop clears it.
func (s *streamer) Err() *streamErr {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *streamer) setErr(err *streamErr) {
	s.errMu.Lock()
	s.err = err
	s.errMu.Unlock()
}

func (s *streamer) dir() string { return filepath.Join(s.root, ".subsonic", "stream") }

func (s *streamer) active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// busy is whether the stream moves data: a song of the window is still to fetch. Waiting for the
// firmware to play on is not busy, so the Q2 may sleep while a list is paused (I14).
func (s *streamer) busy() bool { return s.active() && s.xfer.Load() }

func (s *streamer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	s.setErr(nil)
}

func (s *streamer) stopLocked() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}
}

// clean empties the stream folder but for keep: a new list, or a daemon start, leaves no songs
// from an older one on the card (M26).
func (s *streamer) clean(keep string) {
	ents, _ := os.ReadDir(s.dir())
	for _, e := range ents {
		if p := filepath.Join(s.dir(), e.Name()); p != keep {
			library.Remove(s.root, p)
		}
	}
}

// job is one song of the list: where it plays from, and how to fetch it when it is not synced.
type job struct {
	path    string
	fetch   bool
	quality string
}

// synced reports whether e's file can play from the card: it is there, and no sync is about to
// move or remove it (M24). (The firmware passes over only a real ".." segment, which a Clean path
// has none of, so an ellipsis in a name plays: I19.)
func (s *streamer) synced(e *library.Entry) bool {
	return e != nil && exists(e.Path) && (s.moving == nil || !s.moving(e.ID))
}

// start replaces whatever streams with songs, playing from index from.
func (s *streamer) start(songs []subsonic.Song, from int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	if from < 0 || from >= len(songs) {
		return
	}
	s.setErr(nil)
	stale, _ := os.ReadFile(s.playingFile) // belongs to the previous list
	s.clean("")                            // old queue file, stale and half-fetched songs
	jobs := make([]job, len(songs))
	m := s.manifest()
	for i, sg := range songs {
		if e := m.Songs[sg.ID]; s.caps.Playlist && s.synced(e) {
			jobs[i] = job{path: e.Path}
			continue
		}
		ext, q := strings.ToLower(sg.Suffix), "original"
		if s.caps.Playlist && !library.Playable(ext) || !s.caps.Playlist && ext != "flac" && ext != "mp3" {
			ext, q = "mp3", "mp3-320"
		}
		if s.quality != nil {
			if sq := s.quality(); sq == "mp3-320" || sq == "mp3-128" {
				ext, q = "mp3", sq
			}
		}
		name := fmt.Sprintf("%04d - %s [%s].%s", i, library.SafeName(sg.Title), library.SafeName(sg.ID), ext)
		jobs[i] = job{path: filepath.Join(s.dir(), name), fetch: true, quality: q}
	}
	seq := time.Now().UnixNano() // never reused
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	r := &streamRun{s: s, ctx: ctx, songs: songs, jobs: jobs, from: from, last: from, seq: seq,
		stale: strings.TrimSpace(string(stale)), fails: map[int]int{}, next: map[int]time.Time{}}
	if s.caps.Playlist {
		// Written once, whole: the firmware has queued these lines and waits for missing files.
		var paths []string
		for _, j := range jobs[from:] {
			paths = append(paths, j.path)
		}
		q := filepath.Join(s.dir(), fmt.Sprintf("queue-%d.m3u", seq))
		if err := library.WriteM3U(s.root, q, paths); err != nil {
			cancel()
			close(s.done)
			return
		}
		r.announce(q)
	}
	go func() { defer close(s.done); r.run() }()
}

type streamRun struct {
	s         *streamer
	ctx       context.Context
	songs     []subsonic.Song
	jobs      []job
	from      int
	last      int // the index .playing named last: a song in the list twice is found after it (M25)
	seq       int64
	announced bool
	stale     string // .playing at start: ignored until it changes
	fails     map[int]int
	next      map[int]time.Time
}

func (r *streamRun) announce(path string) {
	if err := q2.WriteFile(r.s.playFile, []byte(fmt.Sprintf("%d %s", r.seq, path))); err == nil {
		r.announced = true
	}
}

// current is the index of the song .playing names, looked for from the last one found on, then back
// from it (a song in the list twice is the occurrence nearest), else the last one; and .playing as read.
func (r *streamRun) current() (int, string) {
	b, _ := os.ReadFile(r.s.playingFile)
	p := strings.TrimSpace(string(b))
	if p == "" || p == r.stale {
		return r.last, p
	}
	for i := r.last; i < len(r.jobs); i++ {
		if r.jobs[i].path == p {
			r.last = i
			return i, p
		}
	}
	for i := r.last - 1; i >= r.from; i-- {
		if r.jobs[i].path == p {
			r.last = i
			return i, p
		}
	}
	return r.last, p
}

func (r *streamRun) run() {
	defer r.s.xfer.Store(false)
	idle := r.s.idle
	if idle == 0 {
		idle = idleAfter
	}
	seen, since := "", time.Now() // .playing as read last, and since when
	for r.ctx.Err() == nil {
		cur, now := r.current()
		if now != seen {
			seen, since = now, time.Now()
		}
		for i := r.from; i < cur-streamWindow; i++ {
			if r.jobs[i].fetch {
				library.Remove(r.s.root, r.jobs[i].path)
			}
		}
		want := -1 // the first song of the window still to fetch
		for i := cur; i < len(r.jobs) && i <= cur+streamWindow; i++ {
			if j := r.jobs[i]; j.fetch && !exists(j.path) {
				want = i
				break
			}
		}
		r.s.xfer.Store(want >= 0)
		fetched := false
		if want >= 0 && !time.Now().Before(r.next[want]) { // else backing off
			if err := r.fetch(r.songs[want], r.jobs[want]); err != nil {
				if r.ctx.Err() != nil {
					return
				}
				r.fails[want]++
				s := r.s
				if s.log != nil {
					s.log.Printf("%s", applog.Redact(fmt.Sprintf("stream %q try %d: %v", r.songs[want].Title, r.fails[want], err)))
				}
				if r.fails[want] >= len(s.backoff) {
					s.setErr(&streamErr{title: r.songs[want].Title, id: r.songs[want].ID, at: time.Now(), err: err})
					return
				}
				r.next[want] = time.Now().Add(s.backoff[r.fails[want]-1])
			} else {
				fetched = true
			}
		}
		if !r.announced && exists(r.jobs[r.from].path) {
			r.announce(r.s.dir()) // stock firmware: plays the folder once it has a song
		}
		// The last song plays, or none has started for the idle time (paused, or the firmware plays
		// something else): nothing more to fetch. What was fetched goes but the song last playing (M26).
		if want < 0 && (cur == len(r.jobs)-1 || time.Since(since) >= idle) {
			if r.jobs[cur].fetch {
				r.s.clean(r.jobs[cur].path)
			} else {
				r.s.clean("")
			}
			return
		}
		if !fetched {
			select {
			case <-r.ctx.Done():
			case <-time.After(r.s.poll):
			}
		}
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// fetch downloads one song to <final>.part, syncs it and renames it, holding the network slot
// with priority so it goes before the syncer's next file. Originals come through PlayURL, which
// servers allow when downloads are turned off (M27, R37).
func (r *streamRun) fetch(sg subsonic.Song, j job) error {
	s := r.s
	if err := s.net.Acquire(r.ctx, true); err != nil {
		return err
	}
	defer s.net.Release()
	resp, err := s.src.Get(r.ctx, s.src.PlayURL(sg.ID, j.quality), 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	part := j.path + ".part"
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return err
	}
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	limit := maxBytes(resp.ContentLength, sg, j.quality)
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if err == nil {
		err = sizeErr(n, resp.ContentLength, limit)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, j.path)
	}
	if err != nil {
		library.Remove(s.root, part)
	}
	return err
}

// maxBytes is the most a song's answer may hold: its Content-Length, else twice what the song
// takes and 8 MiB, so a server that never stops can't fill the card (M9).
// ponytail: unbounded when the server sends neither a length nor a size or duration.
func maxBytes(contentLength int64, sg subsonic.Song, quality string) int64 {
	if contentLength >= 0 {
		return contentLength
	}
	want := sg.Size
	switch quality {
	case "mp3-320":
		want = max(want, int64(sg.Duration)*320000/8)
	case "mp3-128":
		want = max(want, int64(sg.Duration)*128000/8)
	}
	if want <= 0 {
		return 1<<63 - 2
	}
	return 2*want + 8<<20
}

// sizeErr refuses a body of n bytes that is empty, short of its Content-Length (when one came) or
// past limit: it is no song (I8, M9).
func sizeErr(n, contentLength, limit int64) error {
	switch {
	case n == 0:
		return errors.New("the server sent an empty file")
	case contentLength >= 0 && n < contentLength:
		return fmt.Errorf("the server sent %d of %d bytes", n, contentLength)
	case n > limit:
		return fmt.Errorf("the server sent more than the %d bytes expected", limit)
	}
	return nil
}
