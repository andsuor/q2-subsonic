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
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

const streamWindow = 2 // songs fetched ahead of, and kept behind, the one playing

// playableSuffix are the formats the Q2 plays, streamed as they are when the firmware takes playlists.
var playableSuffix = map[string]bool{"flac": true, "mp3": true, "m4a": true, "aac": true, "alac": true, "ogg": true,
	"wav": true, "aiff": true, "aif": true, "ape": true, "wma": true, "dsf": true, "dff": true}

type streamSource interface {
	MediaURL(id, quality string) string
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
	backoff                     []time.Duration // wait after each failed try of a song; one try per entry
	log                         *applog.Log     // optional
	quality                     func() string   // optional Streaming Quality; mp3-320/mp3-128 transcode every streamed song

	errMu sync.Mutex
	err   error

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func newStreamer(root, play, playing string, caps fw.Caps, src streamSource, man func() *library.Manifest,
	net *syncer.NetSlot, poll time.Duration) *streamer {
	return &streamer{root: root, playFile: play, playingFile: playing, caps: caps, src: src, manifest: man, net: net, poll: poll,
		backoff: []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second}}
}

// Err is set when a song failed all its tries; prefetching stops at that song.
func (s *streamer) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *streamer) setErr(err error) {
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

func (s *streamer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *streamer) stopLocked() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}
}

// job is one song of the list: where it plays from, and how to fetch it when it is not synced.
type job struct {
	path    string
	fetch   bool
	quality string
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
	// A new list gets a clean folder (old queue file, stale and half-fetched songs).
	if ents, err := os.ReadDir(s.dir()); err == nil {
		for _, e := range ents {
			library.Remove(s.root, filepath.Join(s.dir(), e.Name()))
		}
	}
	jobs := make([]job, len(songs))
	m := s.manifest()
	for i, sg := range songs {
		if e := m.Songs[sg.ID]; s.caps.Playlist && e != nil {
			jobs[i] = job{path: e.Path}
			continue
		}
		ext, q := strings.ToLower(sg.Suffix), "original"
		if s.caps.Playlist && !playableSuffix[ext] || !s.caps.Playlist && ext != "flac" && ext != "mp3" {
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
	r := &streamRun{s: s, ctx: ctx, songs: songs, jobs: jobs, from: from, seq: seq, stale: strings.TrimSpace(string(stale)),
		fails: map[int]int{}, next: map[int]time.Time{}}
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

// current is the index of the song .playing names, else the first one.
func (r *streamRun) current() int {
	b, _ := os.ReadFile(r.s.playingFile)
	p := strings.TrimSpace(string(b))
	for i := r.from; i < len(r.jobs) && p != "" && p != r.stale; i++ {
		if r.jobs[i].path == p {
			return i
		}
	}
	return r.from
}

func (r *streamRun) run() {
	for r.ctx.Err() == nil {
		cur := r.current()
		for i := r.from; i < cur-streamWindow; i++ {
			if r.jobs[i].fetch {
				library.Remove(r.s.root, r.jobs[i].path)
			}
		}
		fetched, missing := false, false
		for i := cur; i < len(r.jobs) && i <= cur+streamWindow; i++ {
			j := r.jobs[i]
			if !j.fetch || exists(j.path) {
				continue
			}
			missing = true
			if time.Now().Before(r.next[i]) {
				break // backing off
			}
			if err := r.fetch(r.songs[i], j); err != nil {
				if r.ctx.Err() != nil {
					return
				}
				r.fails[i]++
				s := r.s
				if s.log != nil {
					s.log.Printf("%s", applog.Redact(fmt.Sprintf("stream %q try %d: %v", r.songs[i].Title, r.fails[i], err)))
				}
				if r.fails[i] >= len(s.backoff) {
					s.setErr(fmt.Errorf("Couldn't stream %s", r.songs[i].Title))
					return
				}
				r.next[i] = time.Now().Add(s.backoff[r.fails[i]-1])
				break
			}
			fetched, missing = true, false
			break
		}
		if !r.announced && exists(r.jobs[r.from].path) {
			r.announce(r.s.dir()) // stock firmware: plays the folder once it has a song
		}
		if cur == len(r.jobs)-1 && !fetched && !missing {
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
// with priority so it goes before the syncer's next file.
func (r *streamRun) fetch(sg subsonic.Song, j job) error {
	s := r.s
	if err := s.net.Acquire(r.ctx, true); err != nil {
		return err
	}
	defer s.net.Release()
	resp, err := s.src.Get(r.ctx, s.src.MediaURL(sg.ID, j.quality), 0)
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
	_, err = io.Copy(f, resp.Body)
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
