package main

// Tests from the G1 review round, in their own file so the round merges without touching the end of
// device_test.go.

import (
	"path/filepath"
	"strings"
	"testing"
)

// Songs agreeing on their album artist keep it (a server's displayAlbumArtist is trusted); only an
// album whose songs disagree, a compilation on a server without displayAlbumArtist, takes the
// album's artist (review G1, Minor 2).
func TestGetAlbumTrustsConsistentArtist(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2, albumArtist: "Someone Else"}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.mu.Lock()
	p := d.daemon.preview.plan
	d.mu.Unlock()
	for _, w := range p.Add {
		if !strings.HasPrefix(w.Path, filepath.Join(d.card, "Subsonic", "Artist")+"/") {
			t.Errorf("%s", w.Path)
		}
	}
}
