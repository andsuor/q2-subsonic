package subsonic

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

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
		q.Get("v") != "1.13.0" || q.Get("c") != "q2-subsonic" || q.Get("f") != "json" || q.Has("p") {
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
	if u := c.CoverURLSize("al-1", 64); !strings.Contains(u, "/rest/getCoverArt?") || !strings.Contains(u, "id=al-1") || !strings.Contains(u, "size=64") {
		t.Fatal(u)
	}
}

func TestListings(t *testing.T) {
	ctx := context.Background()
	var q url.Values
	_, c := serve(t, "playlists.json", &q)
	if pl, err := c.Playlists(ctx); err != nil || len(pl) != 2 || pl[1].SongCount != 10 || pl[0].CoverArt != "" || pl[1].CoverArt != "pl-p2_0" {
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
	if m := Message(err, "h"); m != "The server answered HTTP 502" {
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

// serveJSON answers every request with body as application/json.
func serveJSON(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func failed(code int) string {
	return fmt.Sprintf(`{"subsonic-response":{"status":"failed","version":"1.15.0","error":{"code":%d,"message":"m"}}}`, code)
}

// I22/I23: version and sign-in method errors get their own words; 41-43 are not "wrong password",
// 40 and 44 are (ErrAuth).
func TestCodeCopy(t *testing.T) {
	for code, want := range map[int]string{
		20: "The server needs a newer version of this app",
		30: "The server's Subsonic API is too old for this app",
		40: "Wrong username or password",
		41: "This server doesn't allow token sign-in for this user",
		42: "This server doesn't accept this sign-in method",
		43: "This server doesn't accept this sign-in method",
		44: "Invalid API key: create one in your server's settings",
	} {
		err := serveJSON(t, failed(code)).Ping(context.Background())
		if m := Message(err, "h"); m != want {
			t.Errorf("code %d: %q, want %q", code, m, want)
		}
		auth, method := errors.Is(err, ErrAuth), errors.Is(err, ErrAuthMethod)
		switch code {
		case 40, 44:
			if !auth || method {
				t.Errorf("code %d kind: %v", code, err)
			}
		case 41, 42, 43:
			if auth || !method {
				t.Errorf("code %d kind: %v", code, err)
			}
		default:
			if auth || method {
				t.Errorf("code %d kind: %v", code, err)
			}
		}
	}
}

// I8: a 200 answer that is a Subsonic error is an error, never saved as a song or a cover.
func TestGetRefusesErrorBody(t *testing.T) {
	ctx := context.Background()
	c := serveJSON(t, failed(40))
	for _, u := range []string{c.MediaURL("s1", "original"), c.MediaURL("s1", "mp3-320"), c.CoverURL("c1")} {
		if resp, err := c.Get(ctx, u, 0); resp != nil || !errors.Is(err, ErrAuth) {
			t.Fatalf("%s: %v %v", u, resp, err)
		}
	}
	c = serveJSON(t, failed(70))
	resp, err := c.Get(ctx, c.MediaURL("s1", "mp3-128"), 0)
	var ae *APIError
	if resp != nil || !errors.As(err, &ae) || ae.Code != 70 || ae.Op != "stream" {
		t.Fatalf("code 70: %v %v", resp, err)
	}
	c = serveJSON(t, `{"subsonic-response":{"status":"ok"}}`) // not an error, but not the file either
	if resp, err := c.Get(ctx, c.CoverURL("c1"), 0); resp != nil || err == nil {
		t.Fatalf("ok envelope as a cover: %v %v", resp, err)
	}
}

// M27/R37: sync respects a server that turns downloads off for the account: a refused original is
// an error with its own words, never fetched another way. Playback opts in to stream?format=raw.
func TestDownloadsOffAndPlayURL(t *testing.T) {
	var streamed url.Values
	var rng string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path.Base(r.URL.Path) == "download" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(failed(50)))
			return
		}
		streamed, rng = r.URL.Query(), r.Header.Get("Range")
		w.Header().Set("Content-Type", "audio/flac")
		w.Write([]byte("AUDIO"))
	}))
	defer srv.Close()
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	ctx := context.Background()

	resp, err := c.Get(ctx, c.MediaURL("s1", "original"), 0)
	var ae *APIError
	if resp != nil || !errors.As(err, &ae) || ae.Op != "download" || ae.Code != 50 ||
		Message(err, "h") != "Downloads are turned off for this account on the server" || streamed != nil {
		t.Fatalf("sync: %v %v (%q), streamed %v", resp, err, Message(err, "h"), streamed)
	}

	if u := c.PlayURL("s1", "mp3-320"); !strings.Contains(u, "/rest/stream?") || !strings.Contains(u, "format=mp3") || !strings.Contains(u, "maxBitRate=320") {
		t.Fatal(u)
	}
	resp, err = c.Get(ctx, c.PlayURL("s1", "original"), 100)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "AUDIO" || streamed.Get("id") != "s1" || streamed.Get("format") != "raw" || streamed.Has("maxBitRate") || rng != "bytes=100-" {
		t.Fatalf("play: body %q query %v range %q", b, streamed, rng)
	}
}

// M5: an answer past 8 MiB is refused with clear words instead of being decoded (about 3x in RAM).
func TestResponseCap(t *testing.T) {
	big := `{"subsonic-response":{"status":"ok","playlist":{"name":"` + strings.Repeat("x", 9<<20) + `"}}}`
	_, _, err := serveJSON(t, big).Playlist(context.Background(), "p1")
	if m := Message(err, "h"); m != "Playlist too large for the Q2" {
		t.Fatalf("%q (%v)", m, err)
	}
}

// M2: a redirect to another host doesn't carry the token and salt along as Referer.
func TestRedirectDropsReferer(t *testing.T) {
	ref := "unset"
	dst := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref = r.Header.Get("Referer")
		w.Write([]byte(`{"subsonic-response":{"status":"ok"}}`))
	}))
	defer dst.Close()
	src := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer src.Close()
	c, _ := New(Creds{Server: src.URL, User: "u", Password: "pw"}, src.Client())
	if err := c.Ping(context.Background()); err != nil || ref != "" {
		t.Fatalf("err %v, Referer %q", err, ref)
	}
}

// M9: a body that stops arriving fails after the idle time, so a stalled server can't hang a sync or
// hold the network slot; time the reader spends away (the syncer yielding to a stream) doesn't count.
func TestStalledBodyFails(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		w.Write([]byte("one"))
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte("two"))
		w.(http.Flusher).Flush()
		if r.URL.Query().Get("id") == "stall" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}))
	defer srv.Close()
	defer close(release)
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	c.idle = 200 * time.Millisecond
	ctx := context.Background()

	resp, err := c.Get(ctx, srv.URL+"/rest/download?id=slow-reader", 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	io.ReadFull(resp.Body, buf)
	time.Sleep(500 * time.Millisecond) // away longer than idle; the data is waiting
	if rest, err := io.ReadAll(resp.Body); err != nil || string(buf)+string(rest) != "onetwo" {
		t.Fatalf("slow reader: %q %q %v", buf, rest, err)
	}
	resp.Body.Close()

	resp, err = c.Get(ctx, srv.URL+"/rest/download?id=stall", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnreachable) || !strings.Contains(Message(err, "h"), "stopped sending") {
			t.Fatalf("stall: %v (%q)", err, Message(err, "h"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled body hangs the reader")
	}
}

// M1: a user:password@ server address works, but its password never reaches log.txt.
func TestUserinfoNotLogged(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"subsonic-response":{"status":"ok"}}`))
	}))
	defer srv.Close()
	c, err := New(Creds{Server: strings.Replace(srv.URL, "https://", "https://alice:proxySecret42@", 1), User: "u", Password: "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/l"
	c.Log = applog.Open(path, 1<<20)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	c.Ping(context.Background())
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "proxySecret42") || !strings.Contains(string(b), "alice") {
		t.Fatalf("log: %s", b)
	}
}

// M8: with the clock unset and no server Date yet (a failed fetch's 30 s back-off), a certificate
// that is "not yet valid" means the time couldn't be read, not that the certificate is bad.
func TestNotYetValidWhileClockUnknown(t *testing.T) {
	srv, _ := serve(t, "ping.json", nil)
	hc := srv.Client()
	tr := hc.Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.Time = func() time.Time { return time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC) }
	hc.Transport = tr
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, hc)
	defer func(f func() bool) { timeKnown = f }(timeKnown)
	timeKnown = func() bool { return false }
	if err := c.Ping(context.Background()); !errors.Is(err, ErrUnreachable) || errors.Is(err, ErrCert) {
		t.Fatalf("clock unknown: %v", err)
	}
	timeKnown = func() bool { return true }
	if err := c.Ping(context.Background()); !errors.Is(err, ErrCert) {
		t.Fatalf("clock known: %v", err)
	}
}

// keyServer lists apiKeyAuthentication when offer is set: KEY belongs to "andreas". Token auth with
// u/pw works too, unless keyOnly (then it is refused with 42, as on LMS). seen gets every request's
// query, with its endpoint as "_op".
func keyServer(t *testing.T, offer, keyOnly bool, seen *[]url.Values) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rec := r.URL.Query()
		rec.Set("_op", path.Base(r.URL.Path))
		*seen = append(*seen, rec)
		w.Header().Set("Content-Type", "application/json")
		ok := func(extra string) {
			fmt.Fprintf(w, `{"subsonic-response":{"status":"ok","version":"1.16.1","openSubsonic":true%s}}`, extra)
		}
		sum := md5.Sum([]byte("pw" + q.Get("s")))
		switch op := path.Base(r.URL.Path); {
		case op == "getOpenSubsonicExtensions":
			if offer {
				ok(`,"openSubsonicExtensions":[{"name":"formPost","versions":[1]},{"name":"apiKeyAuthentication","versions":[1]}]`)
			} else {
				ok(`,"openSubsonicExtensions":[]`)
			}
		case q.Has("u") && q.Has("apiKey"):
			w.Write([]byte(failed(43)))
		case q.Get("apiKey") == "KEY" && op == "tokenInfo":
			ok(`,"tokenInfo":{"username":"andreas"}`)
		case q.Get("apiKey") == "KEY":
			ok("")
		case q.Has("apiKey"):
			w.Write([]byte(failed(44)))
		case keyOnly:
			w.Write([]byte(failed(42)))
		case q.Get("u") == "u" && q.Get("t") == hex.EncodeToString(sum[:]):
			ok("")
		default:
			w.Write([]byte(failed(40)))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// I23/R38: a server offering OpenSubsonic API keys is signed in to with a key (typed as the
// password), and only the key is kept; the user name is the server's. Nothing secret goes to the
// extensions call.
func TestSignInAPIKey(t *testing.T) {
	var seen []url.Values
	srv := keyServer(t, true, false, &seen)
	c, _ := New(Creds{Server: srv.URL, User: "typed", Password: "KEY"}, srv.Client())
	got, err := c.SignIn(context.Background())
	if err != nil || got != (Creds{Server: srv.URL, User: "andreas", APIKey: "KEY"}) {
		t.Fatalf("%+v %v", got, err)
	}
	for _, q := range seen {
		for _, k := range []string{"u", "t", "s", "p", "apiKey"} {
			if q.Get("_op") == "getOpenSubsonicExtensions" && q.Has(k) {
				t.Fatalf("extensions call sent %s: %v", k, q)
			}
		}
	}
	c, _ = New(got, srv.Client())
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q := seen[len(seen)-1]; q.Get("apiKey") != "KEY" || q.Has("u") || q.Has("t") || q.Has("s") || q.Get("v") != "1.13.0" {
		t.Fatalf("key request %v", q)
	}
}

// R38 (amended): on a server offering API keys, a password still works and is kept as before. Token
// auth goes first, so a correct password never travels in clear as ?apiKey=. When both fail, the
// words cover both.
func TestSignInPasswordOnKeyServer(t *testing.T) {
	var seen []url.Values
	srv := keyServer(t, true, false, &seen)
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if got, err := c.SignIn(context.Background()); err != nil || got != (Creds{Server: srv.URL, User: "u", Password: "pw"}) {
		t.Fatalf("password: %+v %v", got, err)
	}
	for _, q := range seen {
		for k, vs := range q {
			if k == "apiKey" || slices.Contains(vs, "pw") {
				t.Fatalf("a correct password went out in clear: %v", q)
			}
		}
	}
	c, _ = New(Creds{Server: srv.URL, User: "u", Password: "nope"}, srv.Client())
	_, err := c.SignIn(context.Background())
	if !errors.Is(err, ErrAuth) || Message(err, "h") != "Wrong username, password or API key" {
		t.Fatalf("both fail: %v (%q)", err, Message(err, "h"))
	}
	// a server taking only keys (token auth refused with 42): the key was the problem
	srv = keyServer(t, true, true, &seen)
	c, _ = New(Creds{Server: srv.URL, User: "u", Password: "nope"}, srv.Client())
	if _, err := c.SignIn(context.Background()); Message(err, "h") != "Invalid API key: create one in your server's settings" {
		t.Fatalf("key-only server: %v (%q)", err, Message(err, "h"))
	}
}

// Without the extension, sign-in is token auth with the password, as before; 42 keeps its copy.
func TestSignInToken(t *testing.T) {
	var seen []url.Values
	srv := keyServer(t, false, false, &seen)
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if got, err := c.SignIn(context.Background()); err != nil || got != (Creds{Server: srv.URL, User: "u", Password: "pw"}) {
		t.Fatalf("%+v %v", got, err)
	}
	srv = keyServer(t, false, true, &seen)
	c, _ = New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if _, err := c.SignIn(context.Background()); Message(err, "h") != "This server doesn't accept this sign-in method" {
		t.Fatalf("42 without the extension: %v", err)
	}
	// a server without getOpenSubsonicExtensions at all (plain Subsonic) signs in the same way
	_, c = serve(t, "ping.json", nil)
	if _, err := c.SignIn(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// R38: the extensions call failing in transport is reported as such, not as a wrong password.
func TestSignInExtensionsUnreachable(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path.Base(r.URL.Path) == "getOpenSubsonicExtensions" {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(failed(40)))
	}))
	defer srv.Close()
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "KEY"}, srv.Client())
	if _, err := c.SignIn(context.Background()); errors.Is(err, ErrAuth) || !errors.Is(err, ErrUnreachable) {
		t.Fatalf("got %v", err)
	}
}

// Review G2 Minor 2: an error envelope with a missing or wrong Content-Type (a proxy) is still caught
// by its first bytes; real media passes untouched.
func TestGetSniffsErrorBody(t *testing.T) {
	var body string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	for _, b := range []string{"\n " + failed(70),
		`<?xml version="1.0" encoding="UTF-8"?><subsonic-response xmlns="http://subsonic.org/restapi" status="failed"><error code="70"/></subsonic-response>`} {
		body = b
		if resp, err := c.Get(context.Background(), c.MediaURL("s1", "mp3-320"), 0); resp != nil || err == nil {
			t.Fatalf("%q: %v %v", b, resp, err)
		}
	}
	body = "fLaC" + strings.Repeat("\x00", 200)
	resp, err := c.Get(context.Background(), c.MediaURL("s1", "original"), 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != body {
		t.Fatalf("media changed: %d bytes", len(got))
	}
}

// Songs pages every song with OpenSubsonic's empty query: two quote characters, as a bare query=
// gives nothing on older Navidrome.
func TestSongsSendsEmptyQuery(t *testing.T) {
	var q url.Values
	_, c := serve(t, "search.json", &q)
	so, err := c.Songs(context.Background(), 1000, 500)
	if err != nil || len(so) != 1 || so[0].AlbumArtist != "Various" {
		t.Fatal(so, err)
	}
	if q.Get("query") != `""` || q.Get("songCount") != "500" || q.Get("songOffset") != "1000" ||
		q.Get("artistCount") != "0" || q.Get("albumCount") != "0" {
		t.Fatal(q)
	}
}

func TestScanStatus(t *testing.T) {
	_, c := serve(t, "scanstatus.json", nil)
	if ls, n, scanning, err := c.ScanStatus(context.Background()); err != nil || ls != "2026-11-10T03:00:00Z" || n != 42 || !scanning {
		t.Fatal(ls, n, scanning, err)
	}
}

// The Discover lists: what each asks for and how its answer is read.
func TestDiscoverLists(t *testing.T) {
	ctx := context.Background()
	var op string
	var q url.Values
	body := ""
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op, q = path.Base(r.URL.Path), r.URL.Query()
		w.Write([]byte(`{"subsonic-response":{"status":"ok","version":"1.16.1",` + body + `}}`))
	}))
	defer srv.Close()
	c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	song := `[{"id":"s1","title":"One"},{"id":"s2","title":"Two"}]`

	body = `"albumList2":{"album":[{"id":"al1","name":"Alb"}]}`
	if al, err := c.AlbumList(ctx, "byGenre", "Hip Hop", 5, 10); err != nil || len(al) != 1 || al[0].Name != "Alb" ||
		op != "getAlbumList2" || q.Get("type") != "byGenre" || q.Get("genre") != "Hip Hop" || q.Get("offset") != "5" || q.Get("size") != "10" {
		t.Fatal(al, err, op, q)
	}
	if _, err := c.AlbumList(ctx, "newest", "", 0, 10); err != nil || q.Has("genre") {
		t.Fatal(err, q)
	}
	body = `"starred2":{"artist":[{"id":"ar1","name":"Art"}],"album":[{"id":"al1","name":"Alb"}],"song":` + song + `}`
	if ar, al, so, err := c.Starred(ctx); err != nil || op != "getStarred2" || len(ar) != 1 || len(al) != 1 || len(so) != 2 || so[1].Title != "Two" {
		t.Fatal(ar, al, so, err, op)
	}
	body = `"genres":{"genre":[{"value":"Rock","albumCount":3,"songCount":30}]}`
	if g, err := c.Genres(ctx); err != nil || op != "getGenres" || len(g) != 1 || g[0] != (Genre{"Rock", 3, 30}) {
		t.Fatal(g, err, op)
	}
	body = `"randomSongs":{"song":` + song + `}`
	if so, err := c.RandomSongs(ctx, 100, "Rock"); err != nil || op != "getRandomSongs" || len(so) != 2 || q.Get("size") != "100" || q.Get("genre") != "Rock" {
		t.Fatal(so, err, op, q)
	}
	if _, err := c.RandomSongs(ctx, 100, ""); err != nil || q.Has("genre") {
		t.Fatal(err, q)
	}
	body = `"topSongs":{"song":` + song + `}`
	if so, err := c.TopSongs(ctx, "Art", 50); err != nil || op != "getTopSongs" || len(so) != 2 || q.Get("artist") != "Art" || q.Get("count") != "50" {
		t.Fatal(so, err, op, q)
	}
	body = `"similarSongs2":{"song":` + song + `}`
	if so, err := c.SimilarSongs(ctx, "ar1", 50); err != nil || op != "getSimilarSongs2" || len(so) != 2 || q.Get("id") != "ar1" || q.Get("count") != "50" {
		t.Fatal(so, err, op, q)
	}
}

// Unsupported is an endpoint the server doesn't have (a bare HTTP 404, 410 or 501), never a
// Subsonic error or a server that is down.
func TestUnsupported(t *testing.T) {
	for status, want := range map[int]bool{501: true, 410: true, 404: true, 502: false, 500: false} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte("This endpoint is not implemented"))
		}))
		c, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
		if _, err := c.TopSongs(context.Background(), "Art", 50); err == nil || Unsupported(err) != want {
			t.Errorf("HTTP %d: Unsupported(%v) = %v", status, err, !want)
		}
		srv.Close()
	}
	c := serveJSON(t, failed(70))
	if _, err := c.SimilarSongs(context.Background(), "ar1", 50); !errors.Is(err, ErrNotFound) || Unsupported(err) {
		t.Fatalf("code 70: %v", err)
	}
	if Unsupported(nil) || Unsupported(errors.New("x")) {
		t.Fatal("not an API error")
	}
}

// Artists keeps the name of the server's index group each artist is in.
func TestArtistsIndex(t *testing.T) {
	c := serveJSON(t, `{"subsonic-response":{"status":"ok","artists":{"index":[
		{"name":"A","artist":[{"id":"1","name":"Abba","albumCount":2}]},
		{"name":"#","artist":[{"id":"2","name":"10cc","albumCount":1}]}]}}}`)
	ars, err := c.Artists(context.Background())
	want := []Artist{{ID: "1", Name: "Abba", AlbumCount: 2, Index: "A"}, {ID: "2", Name: "10cc", AlbumCount: 1, Index: "#"}}
	if err != nil || !slices.Equal(ars, want) {
		t.Fatalf("%+v, %v", ars, err)
	}
}

func lyricsOf(t *testing.T, status int, body string) (string, error) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path.Base(r.URL.Path) != "getLyricsBySongId" || r.URL.Query().Get("id") != "s1" {
			t.Errorf("asked %s", r.URL.Path)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c.Lyrics(context.Background(), "s1")
}

// Lyrics come as .lrc text: the synced entry when there is one, else the first, plain.
func TestLyrics(t *testing.T) {
	list := func(entries string) string {
		return `{"subsonic-response":{"status":"ok","lyricsList":{"structuredLyrics":[` + entries + `]}}}`
	}
	plain := `{"lang":"und","synced":false,"line":[{"value":"One"},{"value":"Two\r\nhalves"}]}`
	synced := `{"lang":"eng","offset":-100,"synced":true,"line":[{"start":0,"value":"It's bugging me"},{"start":62345,"value":"Grating me"},{"start":6000999,"value":"late"}]}`
	for name, c := range map[string]struct{ body, want string }{
		"synced": {list(plain + "," + synced), "[00:00.00]It's bugging me\n[01:02.34]Grating me\n[100:00.99]late\n"},
		"plain":  {list(plain), "One\nTwo halves\n"},
		"none":   {list(""), ""},
		"absent": {`{"subsonic-response":{"status":"ok"}}`, ""},
	} {
		if got, err := lyricsOf(t, 200, c.body); err != nil || got != c.want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	var ae *APIError
	if _, err := lyricsOf(t, 200, `{"subsonic-response":{"status":"failed","error":{"code":70,"message":"no"}}}`); !errors.As(err, &ae) || ae.Code != 70 {
		t.Errorf("code 70: %v", err)
	}
	if _, err := lyricsOf(t, 404, "404 page not found"); err == nil {
		t.Error("404: no error")
	}
}

// Lyrics longer than the firmware reads are cut at a whole line.
func TestLyricsCut(t *testing.T) {
	line := `{"value":"` + strings.Repeat("x", 999) + `"}`
	got, err := lyricsOf(t, 200, `{"subsonic-response":{"status":"ok","lyricsList":{"structuredLyrics":[{"line":[`+
		strings.Repeat(line+",", 99)+line+`]}]}}}`)
	if err != nil || len(got) != 65000 || !strings.HasSuffix(got, "x\n") {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
}

// A play is reported with scrobble: now playing (submission=false) or played, at a time in ms; a
// zero time sends none, and the server takes its own.
func TestScrobble(t *testing.T) {
	var got url.Values
	_, c := serve(t, "ping.json", &got)
	at := time.UnixMilli(1762860000123)
	if err := c.Scrobble(context.Background(), "s1", at, true); err != nil {
		t.Fatal(err)
	}
	if got.Get("id") != "s1" || got.Get("submission") != "true" || got.Get("time") != "1762860000123" {
		t.Fatalf("query %v", got)
	}
	if err := c.Scrobble(context.Background(), "s1", time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	if got.Get("submission") != "false" || got.Has("time") {
		t.Fatalf("query %v", got)
	}
	var ae *APIError
	if err := serveJSON(t, failed(70)).Scrobble(context.Background(), "s1", at, true); !errors.As(err, &ae) || ae.Code != 70 || errors.Is(err, ErrUnreachable) {
		t.Fatalf("got %v", err)
	}
}
