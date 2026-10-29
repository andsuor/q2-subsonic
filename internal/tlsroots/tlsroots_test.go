package tlsroots

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNowFromDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Tue, 27 Oct 2026 10:00:00 GMT")
	}))
	defer srv.Close()
	nowFn = func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) }
	defer func() { nowFn = time.Now }()
	SetDateURL(srv.URL)
	got := Now()
	if got.Year() != 2026 || got.Month() != 10 || got.Day() != 27 {
		t.Fatalf("Now() = %v", got)
	}
}

func TestPool(t *testing.T) {
	if len(Pool().Subjects()) < 100 {
		t.Fatal("pool too small")
	}
}
