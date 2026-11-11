package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
)

// streamed is the path the firmware sends with goto: a streamed song's file, as start names it.
func (d *tdaemon) streamed(id string) string {
	return filepath.Join(d.stream.dir(), "0000 - Song 0 ["+id+"].flac")
}

func TestStreamID(t *testing.T) {
	name := func(title, id string) string { // start's own
		return fmt.Sprintf("%04d - %s [%s].%s", 3, library.SafeName(title), library.SafeName(id), "mp3")
	}
	for path, want := range map[string]string{
		"/c/.subsonic/stream/" + name("Song", "al1.0"):     "al1.0",
		"/c/.subsonic/stream/" + name("Live [x]", "ab-12"): "ab-12",
		"/c/.subsonic/stream/" + name("T", "a]b"):          "a]b",
		"/c/.subsonic/stream/queue-1.m3u":                  "",
		"/c/.subsonic/stream/0000 - Song.flac":             "",
		"/c/.subsonic/stream/0000 - Song [].flac":          "",
		"": "",
	} {
		if got := streamID(path); got != want {
			t.Errorf("streamID(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestGotoAlbum(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	r := d.do("goto", "album", d.streamed("al1.0"))
	if got := strings.Join(titles(r), "|"); got != "Add to Sync|Play|Song 0|Song 1" || r.Status != "" {
		t.Fatalf("rows %q, status %q %q", got, r.Status, r.Message)
	}
	if q := n.query("getAlbum").Get("id"); q != "al1" {
		t.Errorf("getAlbum id %q", q)
	}
	d.do("goto", "album", d.streamed("al1.0"))
	if got := n.reqs("getSong"); got != 1 {
		t.Errorf("%d getSong calls, want 1: the song is kept", got)
	}
	d.do("toggle", "album:al0") // Sync Now leads it, as it leads the album's own page
	if r := d.do("goto", "album", d.streamed("al1.0")); r.Rows[0].Title != "Sync Now" {
		t.Errorf("rows %q", titles(r))
	}
}

func TestGotoArtist(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	r := d.do("goto", "artist", d.streamed("al1.0"))
	if got := strings.Join(titles(r), "|"); !strings.HasSuffix(got, "|Album 000|Album 001") || r.Status != "" {
		t.Fatalf("rows %q, status %q %q", got, r.Status, r.Message)
	}
	if q := n.query("getArtist").Get("id"); q != "ar1" {
		t.Errorf("getArtist id %q", q)
	}

	// an artist of no album of their own (a compilation track, a guest): said, not a lone Add to Sync
	d = newTestDaemon(t, &nav{albums: 2, songsPer: 2, noAlbums: true}, "", true)
	r = d.do("goto", "artist", d.streamed("al1.0"))
	if got := strings.Join(titles(r), "|"); got != "Add to Sync|No albums" {
		t.Errorf("rows %q", got)
	}

	// a server that names no artist id
	n = &nav{albums: 2, songsPer: 2, noArtistID: true}
	d = newTestDaemon(t, n, "", true)
	r = d.do("goto", "artist", d.streamed("al1.0"))
	if got := strings.Join(titles(r), "|"); got != "The server doesn't say this song's artist" || r.Status != "" {
		t.Errorf("rows %q, status %q", got, r.Status)
	}
	if n.reqs("getArtist") != 0 {
		t.Error("getArtist asked without an id")
	}
}

func TestGotoBadPath(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	for what, args := range map[string][]string{
		"a synced song":   {"album", filepath.Join(d.card, "Subsonic", "Artist", "Album 001", "01 - Song 0 [al1.0].flac")},
		"another folder":  {"album", filepath.Join(d.card, ".subsonic", "0000 - Song 0 [al1.0].flac")},
		"no id":           {"album", filepath.Join(d.stream.dir(), "0000 - Song 0.flac")},
		"another kind":    {"x", d.streamed("al1.0")},
		"no path":         {"album"},
		"nothing at all":  {},
		"a relative path": {"artist", "0000 - Song 0 [al1.0].flac"},
	} {
		if r := d.do("goto", args...); r.Status != q2spot.Bad || r.Message != "Not a Subsonic song" {
			t.Errorf("%s: status %q %q, rows %q", what, r.Status, r.Message, titles(r))
		}
	}
	if got := n.reqs("getSong"); got != 0 {
		t.Errorf("%d getSong calls", got)
	}
	out := newTestDaemon(t, n, "", false)
	if r := out.do("goto", "album", out.streamed("al1.0")); r.Status != q2spot.Login {
		t.Errorf("signed out: status %q", r.Status)
	}
	bad := newTestDaemon(t, &nav{errCode: 70}, "", true) // the id didn't map back: the server's answer, as any list's
	if r := bad.do("goto", "album", bad.streamed("nope")); r.Status == q2spot.Bad || len(r.Rows)+len(r.Message) == 0 {
		t.Errorf("server error: status %q %q, rows %q", r.Status, r.Message, titles(r))
	}
}

func TestGotoPaging(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 120}, "", true)
	p := d.streamed("al1.0")
	r := d.do("goto", "album", p)
	if len(r.Rows) != 50 || r.Cursor == "" {
		t.Fatalf("first page: %d rows, cursor %q", len(r.Rows), r.Cursor)
	}
	next := d.do("goto", "album", p, r.Cursor)
	if len(next.Rows) != 50 || next.Rows[0].Title != "Song 48" {
		t.Fatalf("second page: %d rows from %q", len(next.Rows), titles(next)[:1])
	}
}
