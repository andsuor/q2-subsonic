package tlsroots

import (
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func fakeClock(t *testing.T) {
	t.Helper()
	nowFn = func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) }
	SetDateURL("")
	t.Cleanup(func() { nowFn = time.Now; SetDateURL("") })
}

func dateServer(hits *atomic.Int32, tls bool) *httptest.Server {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Date", "Tue, 27 Oct 2026 10:00:00 GMT")
	})
	if tls {
		return httptest.NewTLSServer(h)
	}
	return httptest.NewServer(h)
}

func TestNowFromDate(t *testing.T) {
	fakeClock(t)
	var hits atomic.Int32
	srv := dateServer(&hits, false)
	defer srv.Close()
	SetDateURL(srv.URL)
	got := Now()
	if got.Year() != 2026 || got.Month() != 10 || got.Day() != 27 {
		t.Fatalf("Now() = %v", got)
	}
	Now()
	if hits.Load() != 1 {
		t.Fatalf("offset not cached: %d fetches", hits.Load())
	}
}

func TestNowFromDateOverVerifiedTLS(t *testing.T) {
	var hits atomic.Int32
	srv := dateServer(&hits, true)
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	resp, err := (&http.Client{Transport: dateTransport(roots, "127.0.0.1")}).Head(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// a server whose cert is not in the roots is refused even though expiry is ignored
	if _, err := (&http.Client{Transport: dateTransport(x509.NewCertPool(), "127.0.0.1")}).Head(srv.URL); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}

func TestClientVerifiesCert(t *testing.T) {
	var hits atomic.Int32
	srv := dateServer(&hits, true)
	defer srv.Close()
	bad := &http.Client{Transport: newTransport(x509.NewCertPool(), time.Second)}
	if resp, err := bad.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("cert outside the pool accepted")
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	good := &http.Client{Transport: newTransport(roots, time.Second)}
	resp, err := good.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestNowFetchFailureFallsBack(t *testing.T) {
	fakeClock(t)
	SetDateURL("http://127.0.0.1:1/")
	start := time.Now()
	got := Now()
	if time.Since(start) > 3*time.Second {
		t.Fatal("Now stalled")
	}
	if got.Year() != 2000 {
		t.Fatalf("Now() = %v, want system time", got)
	}
}

func TestNowBackoff(t *testing.T) {
	fakeClock(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header()["Date"] = nil // no Date at all
	}))
	defer srv.Close()
	SetDateURL(srv.URL)
	Now()
	Now()
	if hits.Load() != 1 {
		t.Fatalf("refetched within backoff: %d", hits.Load())
	}
	base := nowFn()
	nowFn = func() time.Time { return base.Add(retryAfter + time.Second) }
	Now()
	if hits.Load() != 2 {
		t.Fatalf("no retry after backoff: %d", hits.Load())
	}
}

func TestPool(t *testing.T) {
	if len(Pool().Subjects()) < 100 {
		t.Fatal("pool too small")
	}
}

// trustTestCert makes the Date fetch trust srv's certificate (httptest's: 127.0.0.1, ::1, example.com).
func trustTestCert(t *testing.T, srv *httptest.Server) {
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	dateRoots = func() *x509.CertPool { return roots }
	t.Cleanup(func() { dateRoots = Pool })
}

// M6: a server given by IP address is checked against the certificate's IPs too: a certificate
// for other names or IPs can't set the clock.
func TestDateFetchChecksIPHost(t *testing.T) {
	fakeClock(t)
	ln, err := net.Listen("tcp", "127.0.0.2:0") // not in httptest's certificate
	if err != nil {
		t.Skip("no 127.0.0.2:", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Mon, 01 Jan 2052 00:00:00 GMT")
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.StartTLS()
	defer srv.Close()
	trustTestCert(t, srv)
	SetDateURL(srv.URL)
	if got := Now(); got.Year() != 2000 {
		t.Fatalf("Now() = %v from a certificate without 127.0.0.2", got)
	}
}

// M7: the Date fetch never follows a redirect (to http:// least of all); the 3xx's own Date is used.
func TestDateFetchNoRedirect(t *testing.T) {
	fakeClock(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Mon, 01 Jan 2052 00:00:00 GMT")
	}))
	defer plain.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Tue, 27 Oct 2026 10:00:00 GMT")
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer srv.Close()
	trustTestCert(t, srv)
	SetDateURL(srv.URL)
	if got := Now(); got.Year() != 2026 {
		t.Fatalf("Now() = %v", got)
	}
}

// M8: while the first Date fetch runs, other callers wait for it rather than get the unset clock
// (which fails their handshake as "Certificate not trusted").
func TestNowWaitsForFetch(t *testing.T) {
	fakeClock(t)
	got := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(got)
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Date", "Tue, 27 Oct 2026 10:00:00 GMT")
	}))
	defer srv.Close()
	SetDateURL(srv.URL)
	go Now()
	<-got
	if now := Now(); now.Year() != 2026 {
		t.Fatalf("second caller: %v", now)
	}
	if !Known() {
		t.Fatal("Known() false once the Date was read")
	}
}
