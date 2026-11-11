package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// newDirect is newEnv on a firmware with URL queues, Streaming: Direct chosen.
func newDirect(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, fw.Caps{Playlist: true, URLs: true})
	e.s.urlsFile = filepath.Join(filepath.Dir(e.play), "q2-subsonic.urls")
	e.s.direct = func() bool { return true }
	return e
}

func (e *env) sync(t *testing.T, id string) string {
	t.Helper()
	p := e.root + "/Subsonic/A/" + id + ".flac"
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("x"), 0o644)
	e.man.Songs[id] = &library.Entry{ID: id, Path: p}
	return p
}

func (e *env) lrc(n int) string {
	b, _ := os.ReadFile(fmt.Sprintf("%s/q2-subsonic.lyrics/%d.lrc", filepath.Dir(e.play), n))
	return string(b)
}

// ended waits for the start to finish and tells whether it named a queue.
func (e *env) ended(t *testing.T) bool {
	t.Helper()
	until(t, "the start to end", func() bool { return !e.s.active() })
	return exists(e.play)
}

// Direct writes the whole list as a Q2URLS/1 queue, card paths for synced songs and server URLs
// for the rest, readable by the app alone, and names it in .play. Only the start song is asked for.
func TestDirectQueueFile(t *testing.T) {
	e := newDirect(t)
	card := e.sync(t, "s0")
	ss := songs(3, "flac")
	ss[1] = subsonic.Song{ID: "s1", Title: "T\t1\r\nx", Artist: "Ar", Album: "Al", CoverArt: "cv", Suffix: "flac", Duration: 61}
	ss[2].Suffix = "wv" // the Q2 can't play it: transcoded
	e.s.start(ss, 1)
	if !e.ended(t) {
		t.Fatal("no .play")
	}
	want := "#Q2URLS/1\tstart=1\n" +
		card + "\tT0\t\t\t\t0\n" +
		"p:s1:original\tT 1  x\tAr\tAl\tc:cv&size=320\t61\n" +
		"p:s2:mp3-320\tT2\t\t\t\t0\n"
	b, _ := os.ReadFile(e.s.urlsFile)
	if string(b) != want {
		t.Fatalf("got %q\nwant %q", b, want)
	}
	if st, _ := os.Stat(e.s.urlsFile); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if p, _ := os.ReadFile(e.play); !strings.HasSuffix(string(p), " "+e.s.urlsFile) {
		t.Fatalf(".play %q", p)
	}
	if got := e.src.got(); len(got) != 1 || got[0] != "p:s1:original" {
		t.Fatalf("asked for %v", got)
	}
	if m, _ := filepath.Glob(e.root + "/.subsonic/stream/*"); len(m) != 0 {
		t.Fatalf("download-first too: %v", m)
	}
	e.s.stop()
	if exists(e.s.urlsFile) {
		t.Fatal("stop left the queue file")
	}
}

// Streaming Quality's MP3 goes for every URL of the queue.
func TestDirectQuality(t *testing.T) {
	e := newDirect(t)
	e.s.quality = func() string { return "mp3-128" }
	e.s.start(songs(1, "flac"), 0)
	e.ended(t)
	if b, _ := os.ReadFile(e.s.urlsFile); !strings.Contains(string(b), "\np:s0:mp3-128\t") {
		t.Fatalf("%q", b)
	}
}

// Without the firmware's word, with the setting off, with every song on the card or with a list the
// firmware would cut, Play is download-first as before.
func TestDirectFallsBack(t *testing.T) {
	long := func(n int, c string) func(id, q string) string {
		return func(id, q string) string { return "https://h/" + id + strings.Repeat(c, n) }
	}
	for name, set := range map[string]func(e *env) []subsonic.Song{
		"no urls word": func(e *env) []subsonic.Song { e.s.caps.URLs = false; return songs(2, "flac") },
		"setting off":  func(e *env) []subsonic.Song { e.s.direct = func() bool { return false }; return songs(2, "flac") },
		"all synced":   func(e *env) []subsonic.Song { e.sync(t, "s0"); e.sync(t, "s1"); return songs(2, "flac") },
		"long URL":     func(e *env) []subsonic.Song { e.src.play = long(1024, "a"); return songs(2, "flac") },
		"braces":       func(e *env) []subsonic.Song { e.src.play = long(210, "}"); return songs(2, "flac") },
		"250 KiB":      func(e *env) []subsonic.Song { e.src.play = long(900, "a"); return songs(300, "flac") },
		"5001 songs":   func(e *env) []subsonic.Song { return songs(5001, "flac") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newDirect(t)
			e.s.start(set(e), 0)
			if _, body := e.queue(t); !strings.HasPrefix(body, "#EXTM3U\n") {
				t.Fatalf("queue %q", body)
			}
			if exists(e.s.urlsFile) {
				t.Fatal("URL queue written")
			}
		})
	}
}

// A queue of 1,023 URL bytes a line is still the firmware's to play.
func TestDirectLongestURL(t *testing.T) {
	e := newDirect(t)
	e.src.play = func(id, q string) string { return strings.Repeat("a", 1023) }
	e.s.start(songs(1, "flac"), 0)
	if !e.ended(t) || !exists(e.s.urlsFile) {
		t.Fatal("not direct")
	}
}

// Signed out while the queue is built: nothing is handed over, as a line without its URL would
// move every index after it.
func TestDirectSignedOut(t *testing.T) {
	e := newDirect(t)
	e.src.play = func(id, q string) string { return "" }
	e.s.start(songs(2, "flac"), 0)
	if e.ended(t) || exists(e.s.urlsFile) || e.s.Err() != nil {
		t.Fatalf("handed over; err %v", e.s.Err())
	}
}

// The start song is asked for first: with the server away nothing is handed over and the root says so.
func TestDirectProbeOffline(t *testing.T) {
	e := newDirect(t)
	e.src.err = errFor(&subsonic.APIError{Op: "stream", Msg: "connection refused"}, "s1")
	e.s.start(songs(3, "flac"), 1)
	if e.ended(t) || exists(e.s.urlsFile) {
		t.Fatal("handed over")
	}
	if err := e.s.Err(); err == nil || err.Error() != "Couldn't stream T1" {
		t.Fatalf("err %v", err)
	}
}

// A start song the server can't send is left out, and the next one starts.
func TestDirectProbeSkips(t *testing.T) {
	e := newDirect(t)
	e.src.err = errFor(&subsonic.APIError{Op: "stream", HTTP: 200, Code: 70, Msg: "Song not found"}, "s1", "s2")
	e.src.lyrics = func(id string) (string, error) { return "L" + id, nil }
	e.s.start(songs(5, "flac"), 1)
	if !e.ended(t) {
		t.Fatal("no .play")
	}
	b, _ := os.ReadFile(e.s.urlsFile)
	want := "#Q2URLS/1\tstart=1\np:s0:original\tT0\t\t\t\t0\np:s3:original\tT3\t\t\t\t0\np:s4:original\tT4\t\t\t\t0\n"
	if string(b) != want {
		t.Fatalf("got %q", b)
	}
	if err := e.s.Err(); err == nil || err.Error() != "Skipped T2: Song not found" {
		t.Fatalf("err %v", err)
	}
	if e.lrc(1) != "Ls3" || e.lrc(2) != "Ls4" { // by the lines kept
		t.Fatalf("lyrics %q %q", e.lrc(1), e.lrc(2))
	}
}

// The third start song in a row that fails is no song's own fault: nothing is handed over.
func TestDirectProbeThirdEnds(t *testing.T) {
	e := newDirect(t)
	e.src.err = errFor(&subsonic.APIError{Op: "stream", HTTP: 404, Msg: "Not Found"}, "s0", "s1", "s2")
	e.s.start(songs(5, "flac"), 0)
	if e.ended(t) || exists(e.s.urlsFile) {
		t.Fatal("handed over")
	}
	if err := e.s.Err(); err == nil || err.Error() != "Couldn't stream T2" {
		t.Fatalf("err %v", err)
	}
}

// A start song on the card needs no server: the queue starts offline, as a download-first one does.
func TestDirectCardStartOffline(t *testing.T) {
	e := newDirect(t)
	e.sync(t, "s0")
	e.src.fail = func(string) bool { return true }
	e.src.lyrics = func(string) (string, error) { return "", errors.New("offline") }
	e.s.start(songs(2, "flac"), 0)
	if !e.ended(t) || len(e.src.got()) != 0 {
		t.Fatalf("asked for %v", e.src.got())
	}
}

// Lyrics land as <line>.lrc from the start song on, the folder emptied first; a song without any
// leaves no file, and the first error ends the asking.
func TestDirectLyrics(t *testing.T) {
	e := newDirect(t)
	dir := filepath.Join(filepath.Dir(e.play), "q2-subsonic.lyrics")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(dir+"/9.lrc", []byte("old"), 0o644)
	e.src.lyrics = func(id string) (string, error) {
		switch id {
		case "s2":
			return "", nil
		case "s4":
			return "", errors.New("not implemented")
		}
		return "L" + id, nil
	}
	e.s.start(songs(6, "flac"), 1)
	e.ended(t)
	if e.lrc(1) != "Ls1" || e.lrc(3) != "Ls3" || exists(dir+"/0.lrc") || exists(dir+"/2.lrc") || exists(dir+"/9.lrc") {
		ents, _ := os.ReadDir(dir)
		t.Fatalf("lyrics %v", ents)
	}
	if got := strings.Join(e.src.lyr, " "); got != "s1 s2 s3 s4" {
		t.Fatalf("asked for %q", got)
	}
}

// The lyrics folder is the playing queue's until another is handed over: a start that ends with
// none, at the probe or at the queue file, leaves it as it was, the new start song's lyrics out.
func TestDirectKeepsLyricsWithoutHandOff(t *testing.T) {
	for name, set := range map[string]func(e *env){
		"probe": func(e *env) {
			e.src.err = errFor(&subsonic.APIError{Op: "stream", Msg: "connection refused"}, "s0")
		},
		"queue file": func(e *env) { os.MkdirAll(e.s.urlsFile+".tmp/x", 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newDirect(t)
			dir := filepath.Join(filepath.Dir(e.play), "q2-subsonic.lyrics")
			os.MkdirAll(dir, 0o755)
			os.WriteFile(dir+"/0.lrc", []byte("old"), 0o644)
			e.src.lyrics = func(id string) (string, error) { return "L" + id, nil }
			set(e)
			e.s.start(songs(2, "flac"), 0)
			if e.ended(t) {
				t.Fatal("handed over")
			}
			if e.lrc(0) != "old" {
				t.Fatalf("lyrics %q", e.lrc(0))
			}
		})
	}
}

// The Streaming row is there only with the firmware's "urls" word; it toggles and is kept.
func TestSettingsStreamMode(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "playlist", true)
	if got := strings.Join(titles(d.do("settings")), "|"); strings.Contains(got, "Streaming: ") {
		t.Fatalf("rows %q", got)
	}
	d = newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "playlist urls", true)
	rows := titles(d.do("settings"))
	if len(rows) < 3 || rows[1] != "Streaming Quality: Original" || rows[2] != "Streaming: Download First" {
		t.Fatalf("rows %q", rows)
	}
	// Direct sends the sign-in with no certificate check: the row opens a page that says so, and
	// only its own row turns Direct on.
	if row, _ := find(d.do("settings"), "Streaming: Download First"); row.Kind != "list" || row.URI != "direct" {
		t.Fatalf("row %+v", row)
	}
	page := d.do("direct")
	if got := strings.Join(titles(page), "|"); !strings.Contains(got, "certificate") || !strings.Contains(got, "sign-in") {
		t.Fatalf("page %q", got)
	}
	if row, ok := find(page, "Turn On Direct"); !ok || row.Kind != "action" || row.URI != "streamm" || d.stream.direct() {
		t.Fatalf("row %+v", row)
	}
	if r := d.do("streamm"); r.Message != "Streaming: Direct" || !d.stream.direct() {
		t.Fatalf("%+v", r)
	}
	if _, ok := find(d.do("settings"), "Streaming: Direct"); !ok {
		t.Fatalf("rows %q", titles(d.do("settings")))
	}
	if _, ok := find(d.do("direct"), "Turn Off Direct"); !ok {
		t.Fatalf("page %q", titles(d.do("direct")))
	}
	if r := d.do("streamm"); r.Message != "Streaming: Download First" || d.stream.direct() {
		t.Fatalf("%+v", r)
	}
	os.Remove(filepath.Join(d.dir, "settings.json"))
	os.Mkdir(filepath.Join(d.dir, "settings.json"), 0o755)
	if r := d.do("streamm"); r.Status != q2spot.Error || r.Message != "Couldn't save the setting" {
		t.Fatalf("%+v", r)
	}
}

// With Direct the daemon hands the firmware the server's own URLs, sign-in included, and none of
// it reaches the log; Sign Out takes the queue file away.
func TestDirectPlayAndSignOut(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "playlist urls", true)
	d.do("streamm")
	d.do("play", "album:al0 1")
	urls := filepath.Join(d.tmp, "q2-subsonic.urls")
	play := filepath.Join(d.tmp, "q2-subsonic.play")
	eventually(t, ".play", func() bool { return exists(play) })
	if p, _ := os.ReadFile(play); !strings.HasSuffix(string(p), " "+urls) {
		t.Fatalf(".play %q", p)
	}
	b, _ := os.ReadFile(urls)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || lines[0] != "#Q2URLS/1\tstart=1" {
		t.Fatalf("%q", lines)
	}
	for i, l := range lines[1:] {
		f := strings.Split(l, "\t")
		if len(f) != 6 || !strings.HasPrefix(f[0], "https://") || !strings.Contains(f[0], "/rest/stream?") ||
			!strings.Contains(f[0], fmt.Sprintf("id=al0.%d", i)) || !strings.Contains(f[0], "&t=") || f[1] != fmt.Sprintf("Song %d", i) || f[5] != "100" {
			t.Fatalf("line %q", f)
		}
	}
	eventually(t, "the start to end", func() bool { return !d.stream.active() })
	lg, _ := os.ReadFile(filepath.Join(d.dir, "log.txt"))
	for _, secret := range []string{"t=", "s=", "apiKey="} {
		for _, f := range strings.FieldsFunc(string(lg), func(r rune) bool { return r == '?' || r == '&' || r == ' ' || r == '\n' }) {
			if strings.HasPrefix(f, secret) && !strings.HasPrefix(f, secret+"***") {
				t.Fatalf("log holds %q", f)
			}
		}
	}
	if !strings.Contains(string(lg), "/rest/stream?") {
		t.Fatalf("the start song wasn't asked for: %s", lg)
	}
	d.do("signout")
	eventually(t, "the queue file to go", func() bool { return !exists(urls) })
}

// A queue file an earlier run left (a kill) goes at the next start.
func TestDirectFileGoneAtStartup(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "playlist urls", true)
	urls := filepath.Join(d.tmp, "q2-subsonic.urls")
	os.WriteFile(urls, []byte("x"), 0o600)
	os.WriteFile(urls+".tmp", []byte("x"), 0o600)
	d.stream.clean() // as newDaemon does
	if exists(urls) || exists(urls+".tmp") {
		t.Fatal("left in /tmp")
	}
}
