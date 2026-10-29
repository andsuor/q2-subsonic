package tlsroots

import (
	"crypto/x509"
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
	resp, err := (&http.Client{Transport: dateTransport(roots)}).Head(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// a server whose cert is not in the roots is refused even though expiry is ignored
	if _, err := (&http.Client{Transport: dateTransport(x509.NewCertPool())}).Head(srv.URL); err == nil {
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
