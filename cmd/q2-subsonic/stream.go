package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// plays reports the songs .playing names to the server.
	plays *plays
	// The stream cache (cache.go). cacheCap nil: none, and streamed files go as the list plays on.
	cacheCap func() int64            // its limit in bytes; 0 is Off
	acct     func() string           // the signed-in account's folder name, "" when signed out
	free     func(path string) int64 // the card's free bytes
	reserve  func() int64            // the bytes a sync running or paused still needs
	clock    func() time.Time        // the app's clock for a kept file's time; it may wait (now)
	cmu      sync.Mutex              // one walk at a time
	kmu      sync.Mutex              // keep, and each delete
	keep     map[string]bool         // the protected songs (protect)
	used     atomic.Int64            // bytes kept, as last walked and fetched since; for Settings, and when to walk
	counted  atomic.Bool             // a walk has set used

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
		gap:     2100 * time.Millisecond, // 2 s and a tick of the kernel's coarse clock
		plays:   &plays{unit: time.Second}}
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
	s.plays.clear() // no play goes to another account
	s.setErr(nil)
}

func (s *streamer) stopLocked() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}
	s.plays.cancel() // the song playing is left; one that counts after its run ended, too
}

// playingNow is .playing, and when the firmware wrote it. Each write is a new file renamed into
// place (blob_io), made only when the song changes or a new list starts: so a new time is a
// song's start, also at the path the file had.
func (s *streamer) playingNow() (string, time.Time) {
	f, err := os.Open(s.playingFile)
	if err != nil {
		return "", time.Time{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", time.Time{}
	}
	b, _ := io.ReadAll(io.LimitReader(f, 4096))
	return strings.TrimSpace(string(b)), st.ModTime()
}

// clean empties the stream folder of its files: a new list, or a daemon start, leaves no uncached
// songs or queue from an older one on the card (M26). The cache's folders stay.
func (s *streamer) clean() {
	ents, _ := os.ReadDir(s.dir())
	for _, e := range ents {
		if !e.IsDir() {
			library.Remove(s.root, filepath.Join(s.dir(), e.Name()))
		}
	}
}

// cleanLeft is clean at a daemon start, but for a stream the firmware may still play: .playing
// names a song that is there, of the folder or of the newest queue file. The app exits when idle
// with the queue still playing, and Go to starts it again from Now Playing. Such a folder goes at
// the next Play, or at a start after the Q2's own restart, which empties /tmp. The queue's songs
// from the one playing on are protected again, as its run had them; only the songs around it when
// it started so long ago that the queue was left, or is paused, as a run's end has it.
// ponytail: the songs' lengths aren't known here, so an hour stands for the one playing; read the
// length from the file if a longer one that still plays loses the songs after its window to a clear.
func (s *streamer) cleanLeft() {
	s.protect(nil)
	if p, at := s.playingNow(); p != "" && exists(p) {
		q := s.lastQueue()
		i := slices.Index(q, p)
		if i >= 0 {
			hi := len(q)
			if time.Since(at) >= cmp.Or(s.idle, idleAfter)+time.Hour {
				hi = min(i+streamWindow+1, hi)
			}
			s.protect(q[max(i-streamWindow, 0):hi])
		}
		if i >= 0 || filepath.Dir(p) == s.dir() {
			return
		}
	}
	s.clean()
}

// job is one song of the list: where it plays from, and how to fetch it when it is not synced.
type job struct {
	path    string
	fetch   bool
	cached  bool // kept in the stream cache: never deleted behind the song playing
	quality string
	fails   int       // failed tries
	next    time.Time // no try before
	landed  bool      // fetched once in this run: the firmware may have passed it
	away    bool      // the server was lost at it: tried on until it answers or the list is left
}

// synced reports whether e's file can play from the card: it is there, and no sync is about to
// move or remove it (M24). (The firmware passes over only a real ".." segment, which a Clean path
// has none of, so an ellipsis in a name plays: I19.)
func (s *streamer) synced(e *library.Entry) bool {
	return e != nil && exists(e.Path) && (s.moving == nil || !s.moving(e.ID))
}

// format is the extension and quality a song not on the card plays at.
func (s *streamer) format(sg subsonic.Song) (ext, q string) {
	ext, q = strings.ToLower(sg.Suffix), "original"
	if s.caps.Playlist && !library.Playable(ext) || !s.caps.Playlist && ext != "flac" && ext != "mp3" {
		ext, q = "mp3", "mp3-320"
	}
	if s.quality != nil {
		if sq := s.quality(); sq == "mp3-320" || sq == "mp3-128" {
			ext, q = "mp3", sq
		}
	}
	return ext, q
}

// streamID is the song id in a streamed file's name, "NNNN - <title> [<id>].<ext>" as start writes
// it, or the cache's "<title> [<id>].<ext>"; "" for any other name.
// ponytail: an id SafeName changed (FAT-reserved characters, over 120 bytes) or one holding " ["
// does not come back; keep path→id in the streamer if a server's ids need it.
func streamID(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	i := strings.LastIndex(name, " [")
	if i < 0 || !strings.HasSuffix(name, "]") {
		return ""
	}
	return name[i+2 : len(name)-1]
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
	stale, staleAt := s.playingNow() // belongs to the previous list
	s.clean()                        // old queue file, stale and half-fetched songs
	m := s.manifest()
	jobs := make([]job, len(songs))
	base := s.cacheBase()
	for i, sg := range songs {
		if e := m.Songs[sg.ID]; s.caps.Playlist && s.synced(e) {
			jobs[i] = job{path: e.Path}
			continue
		}
		ext, q := s.format(sg)
		if p := s.cachePath(base, sg, ext, q); p != "" {
			jobs[i] = job{path: p, fetch: true, quality: q, cached: true}
			continue
		}
		name := fmt.Sprintf("%04d - %s [%s].%s", i, library.SafeName(sg.Title), library.SafeName(sg.ID), ext)
		jobs[i] = job{path: filepath.Join(s.dir(), name), fetch: true, quality: q}
	}
	seq := time.Now().UnixNano() // never reused
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	r := &streamRun{s: s, ctx: ctx, songs: slices.Clone(songs), jobs: jobs, from: from, last: from, seq: seq,
		stale: stale, staleAt: staleAt, heard: staleAt}
	r.protect(from, len(jobs))
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
	quietBy   time.Time // when no song has started for the idle time: a lost server is waited for until then
	last      int       // the index .playing named last: a song in the list twice is found after it (M25)
	seq       int64
	announced bool
	stale     string    // .playing at start: ignored until it is written again
	staleAt   time.Time // when it was written
	heard     time.Time // .playing's time when a song's start was last told to plays
	queue     string    // the playlist's file; "" in folder mode
	wrote     time.Time // when it was last written
	skips     int       // songs skipped since the last one fetched
	keptLo    int       // the first job protected, and how many jobs there were then
	keptN     int
}

// protect holds the list's kept songs from index lo to hi, the last one playable, in the cache.
func (r *streamRun) protect(lo, hi int) {
	var paths []string
	for _, j := range r.jobs[lo:min(hi, len(r.jobs))] {
		if j.cached {
			paths = append(paths, j.path)
		}
	}
	r.keptLo, r.keptN = lo, len(r.jobs)
	r.s.protect(paths)
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

// lost reports whether err is the server out of reach, which may pass. A card that can't be
// written reads as a net.Error too (an errno is one), and waiting mends none of those.
func lost(err error) bool {
	var pe *fs.PathError
	var le *os.LinkError
	return errors.Is(syncer.StopReason(err), syncer.ErrOffline) && !errors.As(err, &pe) && !errors.As(err, &le)
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
	// The server is lost (Wi-Fi dropped, the Q2 slept while paused): the songs already fetched
	// play on, so the song is tried on at the backoff's last pace until the server answers. It
	// ends only when no song has started for the idle time: the list was left.
	if lost(err) && time.Now().Before(r.quietBy) {
		j.away, j.next = true, time.Now().Add(s.backoff[len(s.backoff)-1])
		s.setErr(e)
		return true, false
	}
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
// from it (a song in the list twice is the occurrence nearest), else the last one; and .playing as
// read, with its time.
func (r *streamRun) current() (int, string, time.Time) {
	p, at := r.s.playingNow()
	if p == "" || p == r.stale && at.Equal(r.staleAt) {
		return r.last, p, at
	}
	for i := r.last; i < len(r.jobs); i++ {
		if r.jobs[i].path == p {
			r.last = i
			return i, p, at
		}
	}
	for i := r.last - 1; i >= r.from; i-- {
		if r.jobs[i].path == p {
			r.last = i
			return i, p, at
		}
	}
	return r.last, p, at
}

func (r *streamRun) run() {
	defer r.s.xfer.Store(false)
	idle := r.s.idle
	if idle == 0 {
		idle = idleAfter
	}
	seen, since := "", time.Now() // .playing as read last, and since when
	read := time.Now()            // when .playing was read last
	for r.ctx.Err() == nil {
		cur, now, at := r.current()
		// A start seen now is no older than the last read, whatever the file's time says: the
		// clock may have been set meanwhile.
		late := min(time.Since(at), time.Since(read))
		read = time.Now()
		if now != seen {
			seen, since = now, time.Now()
		}
		// A song of this list started: .playing was written, and names it. Not "now != seen": a
		// list started again at the path .playing had is a start too. A fetch may have kept this
		// loop from seeing it for a while, which the file's time tells.
		if !at.Equal(r.heard) {
			if r.heard = at; now == r.jobs[cur].path {
				r.s.plays.now(r.songs[cur], late)
				if r.jobs[cur].cached { // played now: the last to be evicted
					library.Touch(r.s.root, now, r.s.now())
				}
			}
		}
		// Whether .playing names cur for this list: cur is also where it starts, before the
		// firmware has played anything of it.
		named := now == r.jobs[cur].path && !(now == r.stale && at.Equal(r.staleAt))
		for i := r.from; i < cur-streamWindow; i++ {
			if r.jobs[i].fetch && !r.jobs[i].cached {
				library.Remove(r.s.root, r.jobs[i].path)
			}
		}
		if lo := max(r.from, cur-streamWindow); lo != r.keptLo || len(r.jobs) != r.keptN { // the song changed, or one was skipped
			r.protect(lo, len(r.jobs))
		}
		want := -1 // the first song of the window still to fetch
		for i := cur; i < len(r.jobs) && i <= cur+streamWindow; i++ {
			if j := r.jobs[i]; j.fetch && !exists(j.path) {
				want = i
				break
			}
		}
		// Waiting for a lost server is no transfer: it must not keep the Q2 from standby.
		r.s.xfer.Store(want >= 0 && !r.jobs[want].away)
		r.quietBy = since.Add(idle + time.Duration(r.songs[cur].Duration)*time.Second)
		fetched := false
		if want >= 0 && !time.Now().Before(r.jobs[want].next) { // else backing off
			if err := r.fetch(r.songs[want], r.jobs[want]); err == nil {
				if r.jobs[want].away { // the server is back
					r.jobs[want].away = false
					r.s.setErr(nil)
				}
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
		// A list left or paused no longer holds its songs to come in the cache (.playing can't tell
		// the two apart): a clear, a sign-out or a sync may take them.
		away := want >= 0 && r.jobs[want].away // a lost server ends the run in failed, once it is quiet
		if (want < 0 || away) && time.Since(since) >= long || want < 0 && named && cur == len(r.jobs)-1 {
			r.protect(max(r.from, cur-streamWindow), cur+streamWindow+1)
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
	if s.cacheCap != nil {
		s.trim(r.ctx, expect(sg, j.quality), s.reserve())
	}
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
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
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
	} else if j.cached {
		s.used.Add(n)
		library.Touch(s.root, j.path, s.now()) // the card's clock may be unset; the app's is the server's
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
