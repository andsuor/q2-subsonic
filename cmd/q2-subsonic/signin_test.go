package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/creds"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/tlsroots"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeNavidrome answers ping for user "u" / password "pw" only.
func fakeNavidrome(t *testing.T) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("u") != "u" || r.URL.Query().Get("t") == "" {
			w.Write([]byte(`{"subsonic-response":{"status":"failed","version":"1.16.1","error":{"code":40,"message":"Wrong username or password"}}}`))
			return
		}
		// token check: md5("pw"+salt)
		w.Write([]byte(`{"subsonic-response":{"status":"ok","version":"1.16.1"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type rig struct {
	s    *signin
	clk  *clock
	dir  string
	base string
	addr string
	nd   *httptest.Server
}

func newRig(t *testing.T, opts ...func(*signin, *httptest.Server)) *rig {
	t.Helper()
	nd := fakeNavidrome(t)
	dir := t.TempDir()
	clk := &clock{t: time.Unix(1_000_000, 0)}
	s := &signin{
		dir:       dir,
		state:     &q2.State{Path: filepath.Join(dir, "state")},
		now:       clk.now,
		tick:      5 * time.Millisecond,
		newClient: func(c subsonic.Creds) (*subsonic.Client, error) { return subsonic.New(c, nd.Client()) },
	}
	for _, o := range opts {
		o(s, nd)
	}
	s.start(0)
	t.Cleanup(s.stop)
	return &rig{s: s, clk: clk, dir: dir, base: "http://" + s.ln.Addr().String(), addr: s.ln.Addr().String(), nd: nd}
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (r *rig) post(t *testing.T, path, body string, hdr bool, cookie *http.Cookie) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", r.base+path, strings.NewReader(body))
	if hdr {
		req.Header.Set("X-Q2", "1")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := noRedirect().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, b.String()
}

func cookieOf(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Value != "" {
			return c
		}
	}
	return nil
}

func (r *rig) login(t *testing.T) *http.Cookie {
	resp, _ := r.post(t, "/api/login", `{"pin":"`+r.s.pin+`"}`, true, nil)
	c := cookieOf(resp)
	if c == nil {
		t.Fatalf("login gave %d, no cookie", resp.StatusCode)
	}
	return c
}

func TestSigninNeedsKeyOrPIN(t *testing.T) {
	r := newRig(t)
	resp, err := noRedirect().Get(r.base + "/")
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("no cookie: %v %v", err, resp)
	}
	resp, _ = r.post(t, "/api/signin", `{}`, true, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("signin without cookie: %d", resp.StatusCode)
	}
	resp, err = noRedirect().Get(r.base + "/?k=WRONG")
	if err != nil || cookieOf(resp) != nil {
		t.Fatalf("wrong key set a cookie: %v", err)
	}
	resp, err = noRedirect().Get(r.base + "/?k=" + r.s.key)
	c := cookieOf(resp)
	if err != nil || c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("key: %v %v", err, c)
	}
	// the cookie opens the page
	req, _ := http.NewRequest("GET", r.base+"/", nil)
	req.AddCookie(c)
	resp, _ = noRedirect().Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("page with cookie: %d", resp.StatusCode)
	}
	if pg, _ := io.ReadAll(resp.Body); !strings.Contains(string(pg), "unencrypted") {
		t.Fatal("page does not say the password is sent unencrypted")
	}
	// PIN
	resp, _ = r.post(t, "/api/login", `{"pin":"`+r.s.pin+`"}`, true, nil)
	if resp.StatusCode != 200 || cookieOf(resp) == nil {
		t.Fatalf("pin: %d", resp.StatusCode)
	}
}

func TestPINLockout(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 10; i++ {
		resp, _ := r.post(t, "/api/login", `{"pin":"nope"}`, true, nil)
		if resp.StatusCode != 403 {
			t.Fatalf("try %d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := r.post(t, "/api/login", `{"pin":"`+r.s.pin+`"}`, true, nil)
	if resp.StatusCode != 429 || cookieOf(resp) != nil {
		t.Fatalf("locked: %d", resp.StatusCode)
	}
	r.clk.add(59 * time.Second)
	resp, _ = r.post(t, "/api/login", `{"pin":"`+r.s.pin+`"}`, true, nil)
	if resp.StatusCode != 429 {
		t.Fatalf("still locked: %d", resp.StatusCode)
	}
	r.clk.add(2 * time.Second)
	resp, _ = r.post(t, "/api/login", `{"pin":"`+r.s.pin+`"}`, true, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("after a minute: %d", resp.StatusCode)
	}
}

func TestPostNeedsXQ2(t *testing.T) {
	r := newRig(t)
	c := r.login(t)
	body := `{"server":"` + r.nd.URL + `","user":"u","password":"pw"}`
	resp, _ := r.post(t, "/api/signin", body, false, c)
	if resp.StatusCode != 403 {
		t.Fatalf("no X-Q2: %d", resp.StatusCode)
	}
	resp, _ = r.post(t, "/api/login", `{"pin":"`+r.s.pin+`"}`, false, nil)
	if resp.StatusCode != 403 {
		t.Fatalf("login no X-Q2: %d", resp.StatusCode)
	}
	if _, ok := creds.Load(r.dir); ok {
		t.Fatal("saved")
	}
}

func TestSigninSuccessStops(t *testing.T) {
	r := newRig(t)
	if !r.s.active() {
		t.Fatal("not active")
	}
	st, _ := os.ReadFile(filepath.Join(r.dir, "state"))
	for _, k := range []string{"addr=", "pin=" + r.s.pin, "qr=", "qrsize=", "note="} {
		if !strings.Contains(string(st), k) {
			t.Errorf("state lacks %q:\n%s", k, st)
		}
	}
	c := r.login(t)
	resp, body := r.post(t, "/api/signin", `{"server":"`+r.nd.URL+`","user":"u","password":"pw"}`, true, c)
	var got struct {
		OK      bool
		Message string
	}
	json.Unmarshal([]byte(body), &got)
	if resp.StatusCode != 200 || !got.OK || got.Message != "Signed in as u" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	cr, ok := creds.Load(r.dir)
	if !ok || cr.User != "u" || cr.Password != "pw" || cr.Server != r.nd.URL {
		t.Fatalf("saved %v %v", cr, ok)
	}
	deadline := time.Now().Add(time.Second)
	for {
		conn, err := net.DialTimeout("tcp", r.addr, 100*time.Millisecond)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still open after 1 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.s.active() {
		t.Fatal("still active")
	}
	if ok, msg := r.s.result(); !ok || msg != "Signed in as u" {
		t.Fatalf("result %v %q", ok, msg)
	}
	// The QR page, still open on the Q2, says it worked instead of asking for Wi-Fi.
	st, _ = os.ReadFile(filepath.Join(r.dir, "state"))
	if !strings.Contains(string(st), "note=Signed in as u · Press Return") || !strings.Contains(string(st), "addr=") ||
		!strings.Contains(string(st), "qrsize=") || strings.Contains(string(st), "pin=") {
		t.Errorf("state after success:\n%s", st)
	}
}

func TestSigninTimeout(t *testing.T) {
	r := newRig(t)
	r.clk.add(9*time.Minute + 59*time.Second)
	time.Sleep(50 * time.Millisecond)
	if !r.s.active() {
		t.Fatal("stopped early")
	}
	r.clk.add(2 * time.Second)
	// stop() clears ln first and the state keys just after, so wait for both (generous: loaded CI).
	// M31: the QR page, if still open, says it timed out (beside the code, as it draws notes only
	// there) rather than "Connect the Q2 to Wi-Fi"; the PIN is gone.
	var st []byte
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ = os.ReadFile(filepath.Join(r.dir, "state"))
		done := !r.s.active() && strings.Contains(string(st), "note=Sign-in timed out · Press Return")
		if conn, err := net.DialTimeout("tcp", r.addr, time.Second); err == nil {
			conn.Close()
			done = false
		}
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r.s.active() {
		t.Fatal("still active after 10 min")
	}
	if !strings.Contains(string(st), "note=Sign-in timed out · Press Return") || !strings.Contains(string(st), "addr=") ||
		!strings.Contains(string(st), "qrsize=") || strings.Contains(string(st), "pin=") {
		t.Errorf("state after the timeout:\n%s", st)
	}
	if conn, err := net.DialTimeout("tcp", r.addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("listener still open")
	}
}

func TestSigninWrongPassword(t *testing.T) {
	r := newRig(t)
	// the fake rejects an unknown user; ask for one
	c := r.login(t)
	resp, body := r.post(t, "/api/signin", `{"server":"`+r.nd.URL+`","user":"x","password":"bad"}`, true, c)
	var got struct {
		OK      bool
		Message string
	}
	json.Unmarshal([]byte(body), &got)
	if resp.StatusCode != 200 || got.OK || got.Message != "Wrong username or password" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if _, ok := creds.Load(r.dir); ok {
		t.Fatal("saved")
	}
	if !r.s.active() {
		t.Fatal("stopped on failure")
	}
}

// Sign-in sets the clock-fallback URL before its ping, so a Q2 with an unset clock can sign in.
func TestSigninSetsDateURL(t *testing.T) {
	tlsroots.SetDateURL("")
	var seen string
	r := newRig(t, func(s *signin, nd *httptest.Server) {
		s.newClient = func(c subsonic.Creds) (*subsonic.Client, error) {
			seen = tlsroots.DateURL() // newClient runs just before the ping
			return subsonic.New(c, nd.Client())
		}
	})
	c := r.login(t)
	if resp, body := r.post(t, "/api/signin", `{"server":"`+r.nd.URL+`","user":"u","password":"pw"}`, true, c); resp.StatusCode != 200 {
		t.Fatal(body)
	}
	if seen != r.nd.URL {
		t.Fatalf("date URL before ping %q, want %q", seen, r.nd.URL)
	}
}

// I23: on a server offering OpenSubsonic API keys, the phone's "password" is an API key, and only
// the key is saved (with the server's user name); Account says a key, not a password, is on the card.
func TestSigninAPIKeySavesOnlyKey(t *testing.T) {
	nd := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getOpenSubsonicExtensions"):
			w.Write([]byte(`{"subsonic-response":{"status":"ok","openSubsonicExtensions":[{"name":"apiKeyAuthentication","versions":[1]}]}}`))
		case q.Get("apiKey") == "KEY" && !q.Has("u"):
			w.Write([]byte(`{"subsonic-response":{"status":"ok","tokenInfo":{"username":"andreas"}}}`))
		default:
			w.Write([]byte(`{"subsonic-response":{"status":"failed","error":{"code":44,"message":"Invalid API key"}}}`))
		}
	}))
	defer nd.Close()
	r := newRig(t, func(s *signin, _ *httptest.Server) {
		s.newClient = func(c subsonic.Creds) (*subsonic.Client, error) { return subsonic.New(c, nd.Client()) }
	})
	c := r.login(t)
	_, body := r.post(t, "/api/signin", `{"server":"`+nd.URL+`","user":"typed","password":"KEY"}`, true, c)
	if !strings.Contains(body, "Signed in as andreas") {
		t.Fatal(body)
	}
	cr, ok := creds.Load(r.dir)
	if b, _ := os.ReadFile(filepath.Join(r.dir, "credentials")); !ok || cr.APIKey != "KEY" || cr.Password != "" ||
		cr.User != "andreas" || strings.Contains(string(b), "Password") {
		t.Fatalf("saved %s", b)
	}
}

// I24: a failed phone sign-in shows on the Q2's QR page too, with the code and PIN kept for a retry.
func TestSigninFailureOnQRPage(t *testing.T) {
	r := newRig(t)
	c := r.login(t)
	r.post(t, "/api/signin", `{"server":"`+r.nd.URL+`","user":"x","password":"bad"}`, true, c)
	st, _ := os.ReadFile(filepath.Join(r.dir, "state"))
	for _, k := range []string{"note=Wrong username or password", "pin=" + r.s.pin, "addr=", "qrsize="} {
		if !strings.Contains(string(st), k) {
			t.Errorf("state lacks %q:\n%s", k, st)
		}
	}
}

// M32: a submit after the Q2 restarted sign-in (new key) says what happened and what to do.
func TestSigninRestartedCopy(t *testing.T) {
	r := newRig(t)
	resp, body := r.post(t, "/api/signin", `{}`, true, &http.Cookie{Name: signinCookie, Value: "an-old-key"})
	if resp.StatusCode != 401 || !strings.Contains(body, "Sign-in was restarted on the Q2. Open Sign In from Your Phone again and scan the new code.") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

// M3: the sign-in server caps header size and open connections, and has a read timeout, so a LAN
// host can't pin the Q2's memory or file descriptors while it listens.
func TestSigninServerLimits(t *testing.T) {
	r := newRig(t)
	if r.s.srv.ReadTimeout == 0 || r.s.srv.MaxHeaderBytes == 0 || r.s.srv.MaxHeaderBytes > 16<<10 {
		t.Fatalf("ReadTimeout %v MaxHeaderBytes %d", r.s.srv.ReadTimeout, r.s.srv.MaxHeaderBytes)
	}
	req, _ := http.NewRequest("GET", r.base+"/", nil)
	req.Header.Set("X-Big", strings.Repeat("a", 64<<10))
	if resp, err := noRedirect().Do(req); err == nil && resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("64 KiB header: %d", resp.StatusCode)
	}
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < signinConns; i++ {
		c, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	time.Sleep(50 * time.Millisecond) // let the server accept them
	extra, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
		t.Fatalf("connection past the cap kept open: %v", err)
	}
	for _, c := range held {
		c.Close()
	}
	held = nil
	time.Sleep(50 * time.Millisecond)
	if resp, err := noRedirect().Get(r.base + "/"); err != nil || resp.StatusCode != 401 {
		t.Fatalf("after the held ones closed: %v %v", resp, err)
	}
}
