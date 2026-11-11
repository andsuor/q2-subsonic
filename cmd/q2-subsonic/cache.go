package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// The stream cache: with the firmware's "playlist" word, Download First keeps what it fetched, at
//
//	.subsonic/stream/<account>/<quality>/<x>/<Title> [<id>].<ext>
//
// so a song streamed before plays at once. The account and the quality are folders of the path: a
// file fetched for another one is never the path a list asks for. A song is whole once it has its
// name (.part, fsync, rename), and there is no index: the folders are what is kept, and a file's
// time is when it was last played.
const (
	cacheFloor = 256 << 20 // free card space the cache never eats into
	// cacheShard is the hex digits of the id's hash that name <x>: 16 folders. The firmware lists
	// a queue line's whole folder, a stat a file.
	cacheShard = 1
)

// cacheID is an id a file name holds whole and unmistakably: with a "[" or "]" in ids, two songs
// could share a name. A song with any other id streams uncached, as before.
var cacheID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// acctDir names an account's folder.
func acctDir(server, user string) string {
	h := sha1.Sum([]byte(server + "\x00" + user))
	return hex.EncodeToString(h[:4])
}

// cacheBase is the folder the signed-in account's songs are kept in; "" when nothing is kept:
// Off, signed out, or a firmware that plays the stream folder in name order.
func (s *streamer) cacheBase() string {
	if s.cacheCap == nil || !s.caps.Playlist || s.cacheCap() <= 0 {
		return ""
	}
	a := s.acct()
	if a == "" {
		return ""
	}
	return filepath.Join(s.dir(), a)
}

// cachePath is where sg is kept at quality q under base; "" for a song that is not. The title is
// in lower case: the card's FAT finds a file whatever the case of its name, so after a retag that
// changes only the case the old file would be found under a name that is not the one it has, and
// that name is what the protected set holds and what the firmware looks for in the folder's
// listing (m3u_songs).
func (s *streamer) cachePath(base string, sg subsonic.Song, ext, q string) string {
	if base == "" || !cacheID.MatchString(sg.ID) || library.SafeName(sg.ID) != sg.ID {
		return ""
	}
	h := sha1.Sum([]byte(sg.ID))
	x := hex.EncodeToString(h[:])[:cacheShard]
	return library.WithID(filepath.Join(base, q, x, library.SafeName(strings.ToLower(sg.Title))+"."+ext), sg.ID)
}

// inCache reports whether path is a song kept for the signed-in account.
func (s *streamer) inCache(path string) bool {
	if s.acct == nil {
		return false
	}
	a := s.acct()
	return a != "" && strings.HasPrefix(path, filepath.Join(s.dir(), a)+string(filepath.Separator))
}

// now is the app's clock for a kept file's time, zero when the time isn't known. Unlike plays.clock
// it may wait for the server's time: it is asked in the stream's run only, never in a reply.
func (s *streamer) now() time.Time {
	if s.clock == nil {
		return time.Time{}
	}
	return s.clock()
}

// protect names the songs that are never evicted, cleared or swept: the list playing from a
// couple behind the song playing on, as the firmware has queued every line up to the first one
// missing. The set stays after its run, until the next list; a run that learns its list was left
// cuts it to the songs around the one playing (streamRun.run, cleanLeft).
func (s *streamer) protect(paths []string) {
	keep := make(map[string]bool, len(paths))
	for _, p := range paths {
		keep[p] = true
	}
	s.kmu.Lock()
	s.keep = keep
	s.kmu.Unlock()
}

type cacheFile struct {
	path string
	size int64
	at   time.Time
}

func subdirs(dir string) []string {
	var out []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// cacheFiles lists what is kept, for every account, the song played longest ago first, and counts
// it: regular files at stream/<dir>/<dir>/<dir>/ only, so nothing else of .subsonic/ is ever a
// file to delete. s.cmu is held.
// ponytail: emptied folders stay (51 an account); remove them if accounts ever come and go.
func (s *streamer) cacheFiles() []cacheFile {
	var out []cacheFile
	var sum int64
	for _, a := range subdirs(s.dir()) {
		for _, q := range subdirs(a) {
			for _, x := range subdirs(q) {
				ents, _ := os.ReadDir(x)
				for _, e := range ents {
					if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
						out = append(out, cacheFile{filepath.Join(x, e.Name()), fi.Size(), fi.ModTime()})
						sum += fi.Size()
					}
				}
			}
		}
	}
	slices.SortStableFunc(out, func(a, b cacheFile) int { return a.at.Compare(b.at) })
	s.used.Store(sum)
	s.counted.Store(true)
	return out
}

// held reports whether f is protected: a song of the set, or its download. s.kmu is held.
func (s *streamer) held(f cacheFile) bool { return s.keep[strings.TrimSuffix(f.path, ".part")] }

// drop deletes f unless it is protected, tested now, under the set's lock, and reports whether it
// went. The lock is held a delete long, not a walk: a new list's protect never waits for a clear.
func (s *streamer) drop(f cacheFile) bool {
	s.kmu.Lock()
	defer s.kmu.Unlock()
	if s.held(f) || library.Remove(s.root, f.path) != nil {
		return false
	}
	s.used.Add(-f.size)
	return true
}

// trim makes room before a song of song bytes is fetched: when the cache would pass its limit, or
// the card's free space would fall under the floor plus reserve (a sync's bytes still to come,
// which never count against the limit), the songs played longest ago go, to a sixteenth of the
// limit under either. The folders are walked only then. When nothing may go the song is fetched
// all the same, as before there was a cache: so the floor holds against what the cache keeps, not
// against the list playing. While another walk deletes (a clear, a sync's makeRoom) there is no
// trim, and ctx ends one: a stop of the stream never waits for the cache.
// ponytail: the walk itself (a stat a file) is not cut short; test ctx in cacheFiles if it is slow.
func (s *streamer) trim(ctx context.Context, song, reserve int64) {
	if s.cacheCap == nil || !s.cmu.TryLock() {
		return
	}
	defer s.cmu.Unlock()
	limit, free, used := s.cacheCap(), s.free(s.root), s.used.Load()
	if s.counted.Load() && (used == 0 || used+song <= limit && free >= cacheFloor+song+reserve) {
		return
	}
	for _, f := range s.cacheFiles() {
		if s.used.Load()+song <= limit*15/16 && free >= cacheFloor+song+reserve+limit/16 || ctx.Err() != nil {
			break
		}
		if s.drop(f) {
			free += f.size
		}
	}
}

// makeRoom is the syncer's MakeRoom: a sync comes first, so the cache gives what the sync's need
// bytes lack on the card, limit and floor aside. It reports whether the card has the room then:
// the preview counted what the cache could give when it was made, and a list started since is
// protected.
func (s *streamer) makeRoom(need int64) bool {
	if s.cacheCap == nil {
		return true
	}
	s.cmu.Lock()
	defer s.cmu.Unlock()
	free := s.free(s.root)
	if free < need+planMargin && !(s.counted.Load() && s.used.Load() == 0) {
		for _, f := range s.cacheFiles() {
			if free >= need+planMargin {
				break
			}
			if s.drop(f) {
				free += f.size
			}
		}
	}
	return free >= need
}

// reclaimable is the bytes a sync could take: what is kept and not protected.
func (s *streamer) reclaimable() (n int64) {
	if s.cacheCap == nil {
		return 0
	}
	s.cmu.Lock()
	defer s.cmu.Unlock()
	files := s.cacheFiles()
	s.kmu.Lock()
	defer s.kmu.Unlock()
	for _, f := range files {
		if !s.held(f) {
			n += f.size
		}
	}
	return n
}

// clearCache deletes every account's kept songs but the protected ones, and reports whether any
// stayed.
func (s *streamer) clearCache() (kept bool) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	for _, f := range s.cacheFiles() {
		if !s.drop(f) {
			kept = true
		}
	}
	return kept
}

// sweep is the start-up's walk: it counts what is kept, and deletes what other accounts than acct
// left ("": all of it), but for the protected songs, which the Q2 may still play.
func (s *streamer) sweep(acct string) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	own := filepath.Join(s.dir(), acct) + string(filepath.Separator)
	for _, f := range s.cacheFiles() {
		if acct == "" || !strings.HasPrefix(f.path, own) {
			s.drop(f)
		}
	}
}

// lastQueue is the lines of the newest queue file in the stream folder.
func (s *streamer) lastQueue() []string {
	m, _ := filepath.Glob(filepath.Join(s.dir(), "queue-*.m3u"))
	if len(m) == 0 {
		return nil
	}
	f, err := os.Open(slices.Max(m)) // the names are times, of one length
	if err != nil {
		return nil
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
	return slices.DeleteFunc(strings.Split(string(b), "\n"), func(l string) bool { return l == "" || l[0] == '#' })
}
