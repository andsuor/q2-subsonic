package subsonic

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/tlsroots"
)

// serve answers every request with the named fixture (or raw body when name has no .json).
func serve(t *testing.T, fixture string, got *url.Values) (*httptest.Server, *Client) {
	t.Helper()
	body, err := os.ReadFile("testdata/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			*got = r.URL.Query()
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestPingOK(t *testing.T) {
	_, c := serve(t, "ping.json", nil)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPingWrongPassword(t *testing.T) {
	_, c := serve(t, "error40.json", nil)
	err := c.Ping(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("got %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 40 || ae.Op != "ping" {
		t.Fatalf("APIError: %+v", ae)
	}
	if m := Message(err, "h"); m != "Wrong username or password" {
		t.Fatal(m)
	}
}

func TestHTTPRefused(t *testing.T) {
	if _, err := New(Creds{Server: "http://x.example"}, http.DefaultClient); !errors.Is(err, ErrInsecure) {
		t.Fatalf("got %v", err)
	}
}

func TestUntrustedCert(t *testing.T) {
	srv, _ := serve(t, "ping.json", nil)
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, tlsroots.Client(5e9))
	err := c.Ping(context.Background())
	if !errors.Is(err, ErrCert) {
		t.Fatalf("got %v", err)
	}
	if m := Message(err, "h"); m != "Certificate not trusted" {
		t.Fatal(m)
	}
}

func TestUnreachable(t *testing.T) {
	srv, c := serve(t, "ping.json", nil)
	srv.Close()
	err := c.Ping(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("got %v", err)
	}
	if m := Message(err, "nas.example"); m != "Can't reach nas.example" {
		t.Fatal(m)
	}
}

func TestNotSubsonic(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>hi</html>"))
	}))
	defer srv.Close()
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	err := c.Ping(context.Background())
	if !errors.Is(err, ErrNotSubsonic) {
		t.Fatalf("got %v", err)
	}
	if m := Message(err, "h"); m != "Not a Subsonic server" {
		t.Fatal(m)
	}
}

func TestTokenAuth(t *testing.T) {
	var q url.Values
	_, c := serve(t, "ping.json", &q)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum([]byte("pw" + q.Get("s")))
	if q.Get("u") != "u" || q.Get("t") != hex.EncodeToString(sum[:]) || len(q.Get("s")) != 12 ||
		q.Get("v") != "1.16.1" || q.Get("c") != "q2-subsonic" || q.Get("f") != "json" || q.Has("p") {
		t.Fatalf("bad query %v", q)
	}
}

func TestPlaylistKeepsDuplicates(t *testing.T) {
	_, c := serve(t, "playlist_dup.json", nil)
	p, songs, err := c.Playlist(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Mix" || len(songs) != 3 || songs[0].ID != "s1" || songs[1].ID != "s2" || songs[2].ID != "s1" {
		t.Fatalf("%+v %+v", p, songs)
	}
	if songs[0].AlbumArtist != "Various" || songs[1].AlbumArtist != "Art" || songs[0].Disc != 1 || songs[0].Size != 5000000 {
		t.Fatalf("%+v", songs)
	}
}

func TestMediaURL(t *testing.T) {
	_, c := serve(t, "ping.json", nil)
	u := c.MediaURL("s1", "mp3-320")
	if !strings.Contains(u, "/rest/stream?") || !strings.Contains(u, "format=mp3") || !strings.Contains(u, "maxBitRate=320") || !strings.Contains(u, "estimateContentLength=true") {
		t.Fatal(u)
	}
	if !strings.Contains(c.MediaURL("s1", "mp3-128"), "maxBitRate=128") {
		t.Fatal("128")
	}
	if u := c.MediaURL("s1", "original"); !strings.Contains(u, "/rest/download?") || !strings.Contains(u, "id=s1") || strings.Contains(u, "format=") {
		t.Fatal(u)
	}
	if !strings.Contains(c.CoverURL("mf-s1"), "/rest/getCoverArt?") {
		t.Fatal("cover")
	}
}

func TestListings(t *testing.T) {
	ctx := context.Background()
	var q url.Values
	_, c := serve(t, "playlists.json", &q)
	if pl, err := c.Playlists(ctx); err != nil || len(pl) != 2 || pl[1].SongCount != 10 {
		t.Fatal(pl, err)
	}
	_, c = serve(t, "albumlist.json", &q)
	if al, err := c.Albums(ctx, 20, 10); err != nil || len(al) != 1 || q.Get("type") != "alphabeticalByName" || q.Get("offset") != "20" || q.Get("size") != "10" {
		t.Fatal(al, err, q)
	}
	_, c = serve(t, "album.json", nil)
	if a, ss, err := c.Album(ctx, "al1"); err != nil || a.Name != "Alb" || len(ss) != 2 || ss[1].Disc != 2 {
		t.Fatal(a, ss, err)
	}
	_, c = serve(t, "artists.json", nil)
	if ar, err := c.Artists(ctx); err != nil || len(ar) != 2 || ar[1].Name != "Bob" {
		t.Fatal(ar, err)
	}
	_, c = serve(t, "artist.json", nil)
	if a, al, err := c.Artist(ctx, "ar1"); err != nil || a.Name != "Art" || len(al) != 1 {
		t.Fatal(a, al, err)
	}
	_, c = serve(t, "search.json", &q)
	if ar, al, so, err := c.Search(ctx, "x", 50); err != nil || len(ar) != 1 || len(al) != 1 || len(so) != 1 ||
		q.Get("artistCount") != "50" || q.Get("songOffset") != "50" || q.Get("query") != "x" {
		t.Fatal(ar, al, so, err, q)
	}
	_, c = serve(t, "song.json", nil)
	if s, err := c.Song(ctx, "s1"); err != nil || s.Title != "One" {
		t.Fatal(s, err)
	}
}

func TestGetRange(t *testing.T) {
	var rng string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rng = r.Header.Get("Range") }))
	defer srv.Close()
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	resp, err := c.Get(context.Background(), srv.URL+"/x", 100)
	if err != nil || rng != "bytes=100-" {
		t.Fatal(err, rng)
	}
	resp.Body.Close()
}

func TestErrorAndLogRedacted(t *testing.T) {
	var q url.Values
	srv, c := serve(t, "ping.json", &q)
	path := t.TempDir() + "/l"
	c.Log = applog.Open(path, 1<<20)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "t=***") || !strings.Contains(lines[0], "s=***") ||
		strings.Contains(lines[0], "pw") || strings.Contains(lines[0], q.Get("t")) || strings.Contains(lines[0], q.Get("s")) {
		t.Fatalf("log: %q", b)
	}
	srv.Close()
	err := c.Ping(context.Background())
	if strings.Contains(err.Error(), "pw") || regexp.MustCompile(`[?&][ts]=[0-9a-f]{12}`).MatchString(err.Error()) {
		t.Fatalf("secret in error: %v", err)
	}
}

func TestRedirectToHTTPRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/x", http.StatusFound)
	}))
	defer srv.Close()
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if err := c.Ping(context.Background()); !errors.Is(err, ErrInsecure) {
		t.Fatalf("got %v", err)
	}
}

func TestHTTP502NotSubsonic(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer srv.Close()
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	err := c.Ping(context.Background())
	var ae *APIError
	if errors.Is(err, ErrNotSubsonic) || !errors.Is(err, ErrUnreachable) || !errors.As(err, &ae) || ae.HTTP != 502 {
		t.Fatalf("got %v", err)
	}
	if m := Message(err, "h"); m != "Navidrome answered HTTP 502" {
		t.Fatal(m)
	}
}

func TestGetNon2xx(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	resp, err := c.Get(context.Background(), srv.URL+"/x", 0)
	var ae *APIError
	if resp != nil || !errors.As(err, &ae) || ae.HTTP != 404 {
		t.Fatalf("got %v %v", resp, err)
	}
}
