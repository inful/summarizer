package llm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Retry policy, matching internal/gitlab's: a transient upstream
// failure must not abort a cron run that has already published files
// for earlier projects, and a hostile Retry-After must not park the
// process for a day.
const (
	maxRetries           = 3
	retryAfterCapDefault = 60 * time.Second
	maxBackoff           = 30 * time.Second
)

// retryable reports whether a status is worth another attempt. 4xx other
// than 429 is our fault (bad key, bad payload) and retrying only delays
// the operator's error message.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// retryAfter parses the Retry-After header, which may be delta-seconds
// or an HTTP-date, capped at retryAfterCapDefault. Missing or
// unparseable falls back to exponential backoff.
func retryAfter(h string) time.Duration {
	if h == "" {
		return backoff(1)
	}
	var d time.Duration
	if n, err := strconv.Atoi(h); err == nil && n >= 0 {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = max(time.Until(t), 0)
	} else {
		return backoff(1)
	}
	return min(d, retryAfterCapDefault)
}

// backoff returns the wait before retry attempt n (1-indexed), capped at
// maxBackoff. Jitter is deliberately omitted: these calls are sequential
// within one process, so there is no thundering herd to avoid, and
// deterministic waits keep the tests fast.
func backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := time.Duration(1<<(n-1)) * time.Second
	return min(d, maxBackoff)
}

// doWithRetry issues req, reusing the same *http.Request body buffer on
// each attempt (http.Request bodies are consumed on read). Returns the
// final response; the caller decodes. Non-retryable statuses return
// immediately so error messages surface without delay.
func doWithRetry(ctx context.Context, hc *http.Client, req *http.Request, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= maxRetries+1; attempt++ {
		if attempt > 1 {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
		resp, err := hc.Do(req)
		if err != nil {
			// Transport failure: retry, unless the caller cancelled.
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt > maxRetries {
				break
			}
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		if !retryable(resp.StatusCode) {
			return resp, nil
		}
		wait := retryAfter(resp.Header.Get("Retry-After"))
		if attempt > maxRetries {
			return resp, nil // out of attempts — hand back the real response
		}
		// Drain so the connection can be reused rather than leaked.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// sleep waits for d, or returns early if the context is cancelled. This
// is what makes Ctrl-C on a cron run interrupt a backoff instead of
// waiting it out.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
