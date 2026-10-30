package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	// idle ends a stream when no new song has played for this long after the one playing should
	// have ended, and nothing is left to fetch: the firmware leaves .playing alone once it plays
	// anything else, so this is also how a stream learns that it was left. Default idleAfter.
	idle time.Duration
	// gap is the least time between two writes of a queue file: the firmware reads it again when
	// its mtime's second changes, and FAT32 keeps mtimes in 2 s.
	gap time.Duration

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
		backoff: []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second},
		gap:     2100 * time.Millisecond} // 2 s and a tick of the kernel's coarse clock
}

// streamErr is a song that failed all its tries: what the root shows, and its Details.
type streamErr struct {
	title, id string
	at        time.Time
	err       error // the last try's
	skipped   bool  // dropped from the queue, and the list played on
}

func (e *streamErr) Error() string {
	if e.skipped {
		return "Skipped " + e.title + ": " + subsonic.Message(e.err, "")
	}
	return "Couldn't stream " + e.title
}
func (e *streamErr) Unwrap() error { return e.err }

// Err is set when a song failed all its tries; prefetching stops at that song, unless it was
// skipped. A new list or a stop clears it.
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

func (s *streamer) logf(format string, a ...any) {
	if s.log != nil {
		s.log.Printf("%s", applog.Redact(fmt.Sprintf(format, a...)))
	}
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

// clean empties the stream folder: a new list, or a daemon start, leaves no songs from an older
// one on the card (M26).
func (s *streamer) clean() {
	ents, _ := os.ReadDir(s.dir())
	for _, e := range ents {
		library.Remove(s.root, filepath.Join(s.dir(), e.Name()))
	}
}

// job is one song of the list: where it plays from, and how to fetch it when it is not synced.
type job struct {
	path    string
	fetch   bool
	quality string
	fails   int       // failed tries
	next    time.Time // no try before
	landed  bool      // fetched once in this run: the firmware may have passed it
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
	s.clean()                              // old queue file, stale and half-fetched songs
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
	r := &streamRun{s: s, ctx: ctx, songs: slices.Clone(songs), jobs: jobs, from: from, last: from, seq: seq,
		stale: strings.TrimSpace(string(stale))}
	if s.caps.Playlist {
		r.queue = filepath.Join(s.dir(), fmt.Sprintf("queue-%d.m3u", seq))
		if err := r.writeQueue(); err != nil {
			cancel()
			close(s.done)
			return
		}
		r.announce(r.queue)
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
	stale     string    // .playing at start: ignored until it changes
	queue     string    // the playlist's file; "" in folder mode
	wrote     time.Time // when it was last written
	skips     int       // songs skipped since the last one fetched
}

// writeQueue writes the queue file whole, a line a song from the first played on, aside and
// renamed into place. The firmware waits at a song not there yet and has queued the lines before
// it, so only that song and the ones after it may change.
func (r *streamRun) writeQueue() error {
	var paths []string
	for _, j := range r.jobs[r.from:] {
		paths = append(paths, j.path)
	}
	if err := library.WriteM3U(r.s.root, r.queue, paths); err != nil {
		return err
	}
	r.wrote = time.Now()
	return nil
}

// gone reports the server's own word that a song isn't there (Subsonic 70, HTTP 410), which no
// retry changes. A bare 404 is retried: a proxy or a deploy can answer one for a while.
func gone(err error) bool {
	var ae *subsonic.APIError
	return errors.As(err, &ae) && (ae.Code == 70 || ae.HTTP == 410)
}

// failed counts a failed try of song i and reports whether the run goes on, and whether the song
// was skipped. A song is tried once a backoff entry (in a playlist, once when it is gone). Then,
// in a playlist, a song the server can't send (gone, a transcode that fails, an error for a body)
// is dropped from the list and the queue file, and the firmware plays on past it. Losing the
// server, a refusal for the account, a card that can't be written (StopReason: an errno is a
// net.Error), a third song in a row, or a song that landed before (the firmware may have passed
// its line, which must stay) ends the run at it, the queue waiting there.
func (r *streamRun) failed(i int, err error) (goOn, skipped bool) {
	j, sg, s := &r.jobs[i], r.songs[i], r.s
	j.fails++
	s.logf("stream %q try %d: %v", sg.Title, j.fails, err)
	if j.fails < len(s.backoff) && !(r.queue != "" && gone(err)) {
		j.next = time.Now().Add(s.backoff[j.fails-1])
		return true, false
	}
	e := &streamErr{title: sg.Title, id: sg.ID, at: time.Now(), err: err}
	if r.queue == "" || j.landed || syncer.StopReason(err) != nil || r.skips >= 2 {
		s.setErr(e)
		return false, false
	}
	select {
	case <-r.ctx.Done():
		return false, false
	case <-time.After(s.gap - time.Since(r.wrote)):
	}
	r.jobs, r.songs = slices.Delete(r.jobs, i, i+1), slices.Delete(r.songs, i, i+1)
	if werr := r.writeQueue(); werr != nil {
		s.logf("stream: queue: %v", werr)
		s.setErr(e)
		return false, false
	}
	r.skips++
	e.skipped = true
	s.setErr(e)
	s.logf("stream: skipped %s (%q): %v", sg.ID, sg.Title, err)
	r.last = min(r.last, len(r.jobs)-1)
	return r.from < len(r.jobs), true // nothing left to play ends the run
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
		if want >= 0 && !time.Now().Before(r.jobs[want].next) { // else backing off
			if err := r.fetch(r.songs[want], r.jobs[want]); err == nil {
				fetched, r.skips, r.jobs[want].landed = true, 0, true
			} else if r.ctx.Err() != nil {
				return
			} else if goOn, skipped := r.failed(want, err); !goOn {
				return
			} else if skipped {
				continue // the list changed under cur; and the firmware waits for the next song now
			}
		}
		if !r.announced && exists(r.jobs[r.from].path) {
			r.announce(r.s.dir()) // stock firmware: plays the folder once it has a song
		}
		// The last song plays, or none has started for the idle time after the one playing should
		// have ended (paused, or the firmware plays something else): nothing more to fetch. The songs
		// around it stay, as the firmware has them queued; those behind went above, and the rest go
		// at the next Play or start-up (M26).
		long := idle + time.Duration(r.songs[cur].Duration)*time.Second
		if want < 0 && (cur == len(r.jobs)-1 || time.Since(since) >= long) {
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
	limit := syncer.Limit(resp.ContentLength, expect(sg, j.quality))
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if err == nil {
		err = syncer.SizeErr(n, resp.ContentLength, limit)
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

// expect is about the bytes a song takes at quality: its server size, or for a transcode its
// duration at the bitrate when that is more; 0 when neither is known.
func expect(sg subsonic.Song, quality string) int64 {
	switch quality {
	case "mp3-320":
		return max(sg.Size, int64(sg.Duration)*320000/8)
	case "mp3-128":
		return max(sg.Size, int64(sg.Duration)*128000/8)
	}
	return sg.Size
}
