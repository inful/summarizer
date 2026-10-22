package gitlab

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// noRealSleep swaps the backoff for a no-op so the retry path runs in
// microseconds. Restored by t.Cleanup so a failure cannot leak it.
func noRealSleep(t *testing.T) {
	t.Helper()
	old := sleepFn
	sleepFn = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { sleepFn = old })
}

// TestClient_RetriesTransportErrors: a GitLab pod restarting mid-scan
// drops in-flight connections. That is the ordinary way this failure
// arrives, and it is at least as transient as the 502 from the same
// restart — which has always retried. Before, one dropped connection
// cost one project's entire week:
//
//	502 then 200         attempts=2  err=<nil>
//	connection reset     attempts=1  err=connection reset by peer
//
// doJSON is GET-only (http.MethodGet, hardcoded), so there is no write
// to duplicate and retrying is unconditionally safe.
func TestClient_RetriesTransportErrors(t *testing.T) {
	noRealSleep(t)

	drop := func(w http.ResponseWriter) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = c.(*net.TCPConn).SetLinger(0) // RST, not FIN
		_ = c.Close()
	}
	t.Run("502 then 200", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if hits.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`[]`))
		}))
		defer srv.Close()
		c := New(srv.URL, "tok", 5*time.Second)
		if _, err := c.FetchMRs(context.Background(), "g/p", time.Now().Add(-time.Hour)); err != nil {
			t.Fatalf("502 should have been retried: %v", err)
		}
		if hits.Load() != 2 {
			t.Errorf("attempts = %d, want 2", hits.Load())
		}
	})
	t.Run("dropped connection then 200", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if hits.Add(1) == 1 {
				drop(w)
				return
			}
			_, _ = w.Write([]byte(`[]`))
		}))
		defer srv.Close()
		c := New(srv.URL, "tok", 5*time.Second)
		if _, err := c.FetchMRs(context.Background(), "g/p", time.Now().Add(-time.Hour)); err != nil {
			t.Fatalf("a dropped connection should have been retried: %v", err)
		}
		if hits.Load() != 2 {
			t.Errorf("attempts = %d, want 2", hits.Load())
		}
	})
}

// TestClient_TransportRetryStopsOnCancel: Ctrl-C on a Sunday-night run
// must interrupt the backoff, not sit through three of them.
func TestClient_TransportRetryStopsOnCancel(t *testing.T) {
	old := sleepFn
	sleepFn = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { sleepFn = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = c.(*net.TCPConn).SetLinger(0)
		_ = c.Close()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the request is even issued

	c := New(srv.URL, "tok", 5*time.Second)
	if _, err := c.FetchMRs(ctx, "g/p", time.Now().Add(-time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestClient_TransportRetryGivesUpAfterMaxRetries: a permanently dead
// host must stop, not spin. maxRetries+1 attempts, then the error
// surfaces.
func TestClient_TransportRetryGivesUpAfterMaxRetries(t *testing.T) {
	noRealSleep(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = c.(*net.TCPConn).SetLinger(0)
		_ = c.Close()
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", 5*time.Second)
	_, err := c.FetchMRs(context.Background(), "g/p", time.Now().Add(-time.Hour))
	if err == nil {
		t.Fatal("a host that never answers must surface an error")
	}
	if got, want := hits.Load(), int32(maxRetries+1); got != want {
		t.Errorf("attempts = %d, want %d", got, want)
	}
}
