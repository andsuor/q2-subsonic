package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Browsing (playlists, albums, artists, search, an item's page) and choosing what syncs.

// view is what status subtitles are read from.
type view struct {
	sel selection.Set
	m   *library.Manifest
	res map[string][]subsonic.Song
}

func (d *daemon) view() view {
	sel, _ := d.selection()
	m := d.manifest()
	d.mu.Lock()
	res := make(map[string][]subsonic.Song, len(d.resolved))
	for k, v := range d.resolved {
		res[k] = v
	}
	d.mu.Unlock()
	return view{sel, m, res}
}

// status is an item's sync status: On Q2, Waiting to sync, X of Y songs on Q2, or "" when none of it
// is on the Q2 and it is not selected. total is Y when its songs were never fetched.
// ponytail: an unselected artist never fetched shows no partial status; its songs are found by item key only.
func (v view) status(kind selection.Kind, id string, total int) string {
	key := selection.Key(kind, id)
	on := map[string]bool{}
	for sid, e := range v.m.Songs {
		for _, k := range e.Items {
			if k == key {
				on[sid] = true
			}
		}
	}
	if songs, ok := v.res[key]; ok {
		total = len(songs)
		for _, s := range songs {
			if v.m.Songs[s.ID] != nil {
				on[s.ID] = true
			}
		}
	} else if kind == selection.KindAlbum {
		for _, songs := range v.res {
			for _, s := range songs {
				if s.AlbumID == id && v.m.Songs[s.ID] != nil {
					on[s.ID] = true
				}
			}
		}
	}
	x, selected := len(on), v.sel.Has(kind, id)
	switch {
	case x == 0 && selected:
		return "Waiting to sync"
	case x == 0:
		return ""
	case total == 0 || x >= total:
		return "On Q2"
	}
	return fmt.Sprintf("%d of %d songs on Q2", x, total)
}

func (v view) item(kind selection.Kind, id, name, own string, total int) q2spot.Row {
	return q2spot.Row{Kind: "list", URI: string(kind), Title: name, Subtitle: join(own, v.status(kind, id, total)),
		Context: selection.Key(kind, id), Position: "toggle"}
}

func (v view) song(s subsonic.Song, listKey string, i int) q2spot.Row {
	st := ""
	if v.m.Songs[s.ID] != nil {
		st = "On Q2"
	} else if v.sel.Has(selection.KindSong, s.ID) {
		st = "Waiting to sync"
	}
	return q2spot.Row{Kind: "action", URI: "play", Title: s.Title, Subtitle: join(s.Artist, st),
		Context: fmt.Sprintf("song:%s %s %d", s.ID, listKey, i), Position: "toggle"}
}

// syncNowRow is the shortcut to Sync, present while selected items are not all on the Q2.
// Cheap: read from the selection, manifest and cached songs, never the network.
func (d *daemon) syncNowRow() (q2spot.Row, bool) {
	v := d.view()
	n := 0
	for _, it := range v.sel.Items {
		if v.status(it.Kind, it.ID, 0) != "On Q2" {
			n++
		}
	}
	if n == 0 {
		return q2spot.Row{}, false
	}
	return list("sync", "Sync Now", fmt.Sprintf("%d waiting", n), ""), true
}

// withSyncNow puts the Sync Now row first on a list's first page, pushing the last row to the next.
func (d *daemon) withSyncNow(r q2spot.Reply, cursor string) q2spot.Reply {
	row, ok := d.syncNowRow()
	if !ok || cursor != "" || r.Status != "" {
		return r
	}
	if len(r.Rows) >= q2spot.MaxRows {
		n := q2spot.MaxRows - 1
		r.Rows, r.Cursor = r.Rows[:n], strconv.Itoa(n)
	}
	r.Rows = append([]q2spot.Row{row}, r.Rows...)
	return r
}

func (d *daemon) browse(ctx context.Context, cl *subsonic.Client, op, cursor string) q2spot.Reply {
	v := d.view()
	var rows []q2spot.Row
	switch op {
	case "albums": // paged by Navidrome: one more than a page says whether More… follows
		off, _ := strconv.Atoi(cursor)
		al, err := cl.Albums(ctx, off, q2spot.MaxRows+1)
		if err != nil {
			return d.errRows(err, "")
		}
		var r q2spot.Reply
		if len(al) > q2spot.MaxRows {
			al, r.Cursor = al[:q2spot.MaxRows], strconv.Itoa(off+q2spot.MaxRows)
		}
		for _, a := range al {
			r.Rows = append(r.Rows, v.item(selection.KindAlbum, a.ID, a.Name, a.Artist, a.SongCount))
		}
		if len(r.Rows) == 0 && off == 0 {
			r.Rows = []q2spot.Row{info("No albums")}
		}
		return r
	case "playlists":
		pls, err := cl.Playlists(ctx)
		if err != nil {
			return d.errRows(err, "")
		}
		for _, p := range pls {
			rows = append(rows, v.item(selection.KindPlaylist, p.ID, p.Name, plural(p.SongCount, "song"), p.SongCount))
		}
		if len(rows) == 0 {
			rows = []q2spot.Row{{Kind: "info", Title: "No playlists", Subtitle: "Playlists must be public on the server to show here"}}
		}
	case "artists":
		ars, err := cl.Artists(ctx)
		if err != nil {
			return d.errRows(err, "")
		}
		for _, a := range ars {
			rows = append(rows, v.item(selection.KindArtist, a.ID, a.Name, plural(a.AlbumCount, "album"), 0))
		}
		if len(rows) == 0 {
			rows = []q2spot.Row{info("No artists")}
		}
	}
	return page(rows, cursor)
}

// itemPage is an artist's, album's or playlist's own page: Add/Remove, Quality, Play, then its contents.
func (d *daemon) itemPage(ctx context.Context, cl *subsonic.Client, arg, cursor string) q2spot.Reply {
	kind, id := parseKey(arg)
	key := selection.Key(kind, id)
	var songs []subsonic.Song
	var albums []subsonic.Album
	var err error
	switch kind {
	case selection.KindAlbum:
		_, songs, err = getAlbum(ctx, cl, id)
	case selection.KindPlaylist:
		_, songs, err = cl.Playlist(ctx, id)
	case selection.KindArtist:
		_, albums, err = cl.Artist(ctx, id)
	default:
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	if err != nil {
		return d.errRows(err, "")
	}
	if songs != nil {
		d.mu.Lock()
		d.resolved[key] = songs
		d.mu.Unlock()
	}
	v := d.view()
	rows := []q2spot.Row{act("toggle", "Add to Sync", key)}
	for _, it := range v.sel.Items {
		if it.Kind == kind && it.ID == id {
			rows[0].Title = "Remove from Sync"
			rows = append(rows, act("quality", "Quality: "+qualityName(it.Quality), key))
		}
	}
	if len(songs) > 0 {
		rows = append(rows, act("play", "Play", key+" 0"))
	}
	for i, s := range songs {
		rows = append(rows, v.song(s, key, i))
	}
	for _, a := range albums {
		year := ""
		if a.Year > 0 {
			year = strconv.Itoa(a.Year)
		}
		rows = append(rows, v.item(selection.KindAlbum, a.ID, a.Name, year, a.SongCount))
	}
	return page(rows, cursor)
}

// search lists what Navidrome found, artists then albums then songs.
// ponytail: one search3 call, 50 of each kind; page past that when someone needs more.
func (d *daemon) search(ctx context.Context, cl *subsonic.Client, term, cursor string) q2spot.Reply {
	ars, als, songs, err := cl.Search(ctx, term, 0)
	if err != nil {
		return d.errRows(err, "")
	}
	v := d.view()
	var rows []q2spot.Row
	for _, a := range ars {
		rows = append(rows, v.item(selection.KindArtist, a.ID, a.Name, plural(a.AlbumCount, "album"), 0))
	}
	for _, a := range als {
		rows = append(rows, v.item(selection.KindAlbum, a.ID, a.Name, a.Artist, a.SongCount))
	}
	for _, s := range songs {
		rows = append(rows, v.song(s, "song:"+s.ID, 0))
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("No results")}
	}
	return page(rows, cursor)
}

// getAlbum is cl.Album with each song's album artist taken from the album where the server sent
// none (no OpenSubsonic displayAlbumArtist, so the song's own artist stood in): a compilation stays
// one folder, not one per track artist (M41).
// ponytail: here, not in subsonic.Client.Album, which another change owns; move it there later.
func getAlbum(ctx context.Context, cl *subsonic.Client, id string) (subsonic.Album, []subsonic.Song, error) {
	a, songs, err := cl.Album(ctx, id)
	for i, s := range songs {
		if a.Artist != "" && s.AlbumArtist == s.Artist {
			songs[i].AlbumArtist = a.Artist
		}
	}
	return a, songs, err
}

// songsOf fetches a selected item's songs (an artist's: every album's), and keeps them for the
// status subtitles.
func (d *daemon) songsOf(ctx context.Context, cl *subsonic.Client, kind selection.Kind, id string) ([]subsonic.Song, string, error) {
	var songs []subsonic.Song
	var name string
	switch kind {
	case selection.KindPlaylist:
		p, s, err := cl.Playlist(ctx, id)
		if err != nil {
			return nil, "", err
		}
		songs, name = s, p.Name
	case selection.KindAlbum:
		a, s, err := getAlbum(ctx, cl, id)
		if err != nil {
			return nil, "", err
		}
		songs, name = s, a.Name
	case selection.KindArtist:
		a, als, err := cl.Artist(ctx, id)
		if err != nil {
			return nil, "", err
		}
		name = a.Name
		for _, al := range als {
			_, s, err := getAlbum(ctx, cl, al.ID)
			if err != nil {
				return nil, "", err
			}
			songs = append(songs, s...)
		}
	case selection.KindSong:
		s, err := cl.Song(ctx, id)
		if err != nil {
			return nil, "", err
		}
		songs, name = []subsonic.Song{s}, s.Title
	default:
		return nil, "", errors.New("unknown kind " + string(kind))
	}
	if songs == nil {
		songs = []subsonic.Song{}
	}
	d.mu.Lock()
	d.resolved[selection.Key(kind, id)] = songs
	d.mu.Unlock()
	return songs, name, nil
}

// toggle adds or removes an item; one already inside a selected item is left as it is.
func (d *daemon) toggle(ctx context.Context, cl *subsonic.Client, arg string) q2spot.Reply {
	kind, id := parseKey(arg)
	sel, err := d.selection()
	if err != nil {
		return q2spot.Fail(q2spot.Error, "Couldn't read the selection")
	}
	if kind != selection.KindLibrary && sel.Has(selection.KindLibrary, "all") {
		return q2spot.Reply{Message: "Included via Entire Library"}
	}
	if sel.Has(kind, id) {
		sel.Toggle(selection.Item{Kind: kind, ID: id})
		if err := d.saveSelection(sel); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the selection")
		}
		return q2spot.Reply{Message: "Removed from Sync"}
	}
	if kind == selection.KindLibrary {
		sel.Toggle(selection.Item{Kind: kind, ID: id, Name: "Entire Library", Quality: d.settings().def()})
		if err := d.saveSelection(sel); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the selection")
		}
		return q2spot.Reply{Message: "Added to Sync · Entire Library"}
	}
	var name string
	var n int
	var song subsonic.Song
	switch kind {
	case selection.KindArtist: // counted from its albums, without fetching each
		a, als, err := cl.Artist(ctx, id)
		if err != nil {
			return d.errToast(err)
		}
		name, n = a.Name, 0
		for _, al := range als {
			n += al.SongCount
		}
	case selection.KindAlbum, selection.KindPlaylist, selection.KindSong:
		songs, nm, err := d.songsOf(ctx, cl, kind, id)
		if err != nil {
			return d.errToast(err)
		}
		name, n = nm, len(songs)
		if kind == selection.KindSong && len(songs) == 1 {
			song = songs[0]
		}
	default:
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	if via := d.coveredBy(ctx, cl, sel, kind, id, song); via != "" {
		return q2spot.Reply{Message: "Included via " + via}
	}
	sel.Toggle(selection.Item{Kind: kind, ID: id, Name: name, Quality: d.settings().def()})
	if err := d.saveSelection(sel); err != nil {
		return q2spot.Fail(q2spot.Error, "Couldn't save the selection")
	}
	return q2spot.Reply{Message: "Added to Sync · " + plural(n, "song")}
}

// coveredBy names a selected item that already holds this album or song, or "".
func (d *daemon) coveredBy(ctx context.Context, cl *subsonic.Client, sel selection.Set, kind selection.Kind, id string, s subsonic.Song) string {
	album := id
	if kind == selection.KindSong {
		album = s.AlbumID
	} else if kind != selection.KindAlbum {
		return ""
	}
	for _, it := range sel.Items {
		switch {
		case it.Kind == selection.KindAlbum && it.ID == album:
			return it.Name
		case it.Kind == selection.KindArtist:
			if _, als, err := cl.Artist(ctx, it.ID); err == nil {
				for _, a := range als {
					if a.ID == album {
						return it.Name
					}
				}
			}
		case it.Kind == selection.KindPlaylist && kind == selection.KindSong:
			d.mu.Lock()
			songs, ok := d.resolved[selection.Key(it.Kind, it.ID)]
			d.mu.Unlock()
			if !ok {
				songs, _, _ = d.songsOf(ctx, cl, it.Kind, it.ID)
			}
			for _, x := range songs {
				if x.ID == s.ID {
					return it.Name
				}
			}
		}
	}
	return ""
}

func (d *daemon) quality(arg string) q2spot.Reply {
	kind, id := parseKey(arg)
	sel, err := d.selection()
	if err != nil {
		return q2spot.Fail(q2spot.Error, "Couldn't read the selection")
	}
	for _, it := range sel.Items {
		if it.Kind == kind && it.ID == id {
			q := nextQuality(it.Quality)
			sel.SetQuality(kind, id, q)
			if err := d.saveSelection(sel); err != nil {
				return q2spot.Fail(q2spot.Error, "Couldn't save the selection")
			}
			return q2spot.Reply{Message: "Quality: " + qualityName(q)}
		}
	}
	return q2spot.Reply{}
}

// play streams a list from an index ("kind:id index", after a song row's own "song:<id>").
func (d *daemon) play(ctx context.Context, cl *subsonic.Client, arg string) q2spot.Reply {
	f := strings.Fields(arg)
	if len(f) == 3 {
		f = f[1:]
	}
	if len(f) != 2 {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	i, _ := strconv.Atoi(f[1])
	kind, id := parseKey(f[0])
	songs, _, err := d.songsOf(ctx, cl, kind, id)
	if err != nil {
		return d.errToast(err)
	}
	if i < 0 || i >= len(songs) {
		return q2spot.Fail(q2spot.Unavailable, "Nothing to play")
	}
	// R27: starting stops the last stream, which may be mid-fetch; reply now, start behind it.
	d.mu.Lock()
	d.playSeq++
	seq := d.playSeq
	d.mu.Unlock()
	d.spawn(func() {
		d.playMu.Lock()
		defer d.playMu.Unlock()
		d.mu.Lock()
		latest := seq == d.playSeq
		d.mu.Unlock()
		if latest {
			d.stream.start(songs, i)
		}
	})
	return q2spot.Reply{Message: "Playing " + songs[i].Title}
}
