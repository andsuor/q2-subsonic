// Package plan computes the sync preview: what to add, replace, rename and remove so the card
// mirrors the selection. Compute is pure (no I/O, no clock) and deterministic.
package plan

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

const (
	margin     = 16 << 20
	coverBytes = 256 << 10 // a cover.jpg at 600 px, counted once per new album folder
)

type Want struct {
	Song    subsonic.Song
	Quality string
	Path    string
	Items   []string
	Bytes   int64
}

type Move struct{ From, To, ID string }

type Plan struct {
	Add, Replace         []Want
	Rename               []Move
	Remove               []*library.Entry
	Playlists            map[string][]string
	PlaylistsStale       bool // recorded .m3u files hold old paths: rewrite them all, even with no song change
	NeedBytes, FreeBytes int64
	ReplaceBytes         int64 // what the replaces add, or free when negative
	Fits                 bool
}

func rank(q string) int {
	switch q {
	case "original":
		return 3
	case "mp3-320":
		return 2
	case "mp3-128":
		return 1
	}
	return 0
}

// Better reports whether quality a ranks above b (original > mp3-320 > mp3-128).
func Better(a, b string) bool { return rank(a) > rank(b) }

func estimate(s subsonic.Song, q string) int64 {
	switch q {
	case "mp3-320":
		return int64(s.Duration) * 320 * 1000 / 8
	case "mp3-128":
		return int64(s.Duration) * 128 * 1000 / 8
	}
	return s.Size
}

// resized reports whether an original's server file changed size since it was synced (a re-rip).
func resized(e *library.Entry, w *Want) bool {
	return w.Quality == "original" && e.SrcSize > 0 && w.Song.Size > 0 && e.SrcSize != w.Song.Size
}

// Compute diffs the selection (resolved: item key -> its songs; playlistNames: playlist item key
// -> display name) against the manifest.
func Compute(sel selection.Set, resolved map[string][]subsonic.Song, playlistNames map[string]string,
	m *library.Manifest, root string, free int64) Plan {
	wants := map[string]*Want{}
	multi := map[string]bool{}
	for _, it := range sel.Items {
		key := selection.Key(it.Kind, it.ID)
		q := it.Quality
		if q == "" {
			q = "original"
		}
		for _, s := range resolved[key] {
			if s.Disc > 1 {
				multi[s.AlbumID] = true
			}
			q := q
			if q == "original" && !library.Playable(s.Suffix) {
				q = "mp3-320" // a format the Q2 can't play, or no usable extension: as the streamer does
			}
			w := wants[s.ID]
			if w == nil {
				w = &Want{Song: s, Quality: q}
				wants[s.ID] = w
			} else if Better(q, w.Quality) {
				w.Quality = q
			}
			if n := len(w.Items); n == 0 || w.Items[n-1] != key {
				seen := false
				for _, k := range w.Items {
					seen = seen || k == key
				}
				if !seen {
					w.Items = append(w.Items, key)
				}
			}
		}
	}

	ids := make([]string, 0, len(wants))
	for id := range wants {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// unchanged: the song stays as it is on the card.
	unchanged := func(e *library.Entry, w *Want) bool {
		return e.Quality == w.Quality && e.Meta == library.MetaKey(w.Song, w.Quality) && !library.StaleName(root, e.Path) &&
			!resized(e, w)
	}
	// Every song owns its current path, case-folded (the card is FAT/exFAT: case variants are one
	// file), for this whole sync: nothing new lands on a file that a rename, a replace or the removals
	// take away, and no rename lands on another song's file (swapped tags get their [id] instead).
	claim := map[string]string{}
	for id, e := range m.Songs {
		claim[strings.ToLower(e.Path)] = id
	}

	var p Plan
	final := map[string]string{} // song id -> path it will have
	for _, id := range ids {
		w := wants[id]
		e := m.Songs[id]
		if e != nil && unchanged(e, w) {
			w.Path = e.Path
			final[id] = e.Path
			continue
		}
		path := library.SongPath(root, w.Song, w.Quality, multi[w.Song.AlbumID])
		if owner, ok := claim[strings.ToLower(path)]; ok && owner != id {
			path = library.WithID(path, id)
		}
		claim[strings.ToLower(path)] = id
		w.Path, final[id] = path, path
		w.Bytes = estimate(w.Song, w.Quality)
		switch {
		case e == nil:
			p.Add = append(p.Add, *w)
			p.NeedBytes += w.Bytes
		case e.Quality != w.Quality || !strings.EqualFold(filepath.Ext(e.Path), filepath.Ext(path)) || resized(e, w):
			// a new format or a re-rip is fetched again, never renamed
			p.Replace = append(p.Replace, *w)
			p.NeedBytes += w.Bytes - e.Size
			p.ReplaceBytes += w.Bytes - e.Size
		case e.Path != path:
			p.Rename = append(p.Rename, Move{From: e.Path, To: path, ID: id})
		}
	}
	for id, e := range m.Songs {
		if wants[id] == nil {
			p.Remove = append(p.Remove, e)
		}
	}
	folders := map[string]bool{} // album folders on the card, which have their cover
	for _, e := range m.Songs {
		folders[strings.ToLower(filepath.Dir(e.Path))] = true
	}
	for _, w := range append(append([]Want(nil), p.Add...), p.Replace...) {
		if d := strings.ToLower(filepath.Dir(w.Path)); w.Song.CoverArt != "" && !folders[d] {
			folders[d] = true
			p.NeedBytes += coverBytes
		}
	}

	p.Playlists = map[string][]string{}
	p.PlaylistsStale = m.PlaylistsStale
	for _, it := range sel.Items {
		if it.Kind != selection.KindPlaylist {
			continue
		}
		key := selection.Key(it.Kind, it.ID)
		paths := []string{}
		for _, s := range resolved[key] {
			paths = append(paths, final[s.ID])
		}
		p.Playlists[playlistNames[key]] = paths
	}

	byPath := func(a []Want) { sort.Slice(a, func(i, j int) bool { return a[i].Path < a[j].Path }) }
	byPath(p.Add)
	byPath(p.Replace)
	sort.Slice(p.Rename, func(i, j int) bool { return p.Rename[i].To < p.Rename[j].To })
	sort.Slice(p.Remove, func(i, j int) bool { return p.Remove[i].Path < p.Remove[j].Path })
	p.FreeBytes = free
	p.Fits = p.NeedBytes <= 0 || p.NeedBytes+margin <= free // freeing space always fits
	return p
}
