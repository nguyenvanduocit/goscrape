package scraper

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cornelk/gotokit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeaders(t *testing.T) {
	headers := Headers([]string{"a:b", "c:d:e"})
	assert.Equal(t, "b", headers.Get("a"))
	assert.Equal(t, "d:e", headers.Get("c"))
}

func TestDownloadURLWithRetries(t *testing.T) {
	ctx := context.Background()
	expected := "ok"

	var retry int
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retry < maxRetries {
			retry++
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, err := fmt.Fprint(w, expected)
		assert.NoError(t, err)
	}))
	defer svr.Close()

	ur, err := url.Parse(svr.URL)
	require.NoError(t, err)

	origMaxRetries := maxRetries
	origRetryDelay := retryDelay
	t.Cleanup(func() {
		maxRetries = origMaxRetries
		retryDelay = origRetryDelay
	})

	maxRetries = 2
	retryDelay = time.Millisecond

	var cfg Config
	logger := log.NewTestLogger(t)
	s, err := New(logger, cfg)
	require.NoError(t, err)

	// download works after 2 retries
	b, urActual, err := s.downloadURLWithRetries(ctx, ur)
	require.NoError(t, err)
	require.NotNil(t, urActual)
	assert.Equal(t, svr.URL, urActual.String())
	assert.Equal(t, expected, string(b))
	assert.Equal(t, retry, maxRetries)

	// download fails after 3 retries
	retry = -100
	_, _, err = s.downloadURLWithRetries(ctx, ur)
	assert.ErrorIs(t, err, errExhaustedRetries)
}

func TestDownloadURLWithRetries_403NoRetry(t *testing.T) {
	ctx := context.Background()

	var requestCount int64
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer svr.Close()

	ur, err := url.Parse(svr.URL)
	require.NoError(t, err)

	origMaxRetries := maxRetries
	origRetryDelay := retryDelay
	t.Cleanup(func() {
		maxRetries = origMaxRetries
		retryDelay = origRetryDelay
	})
	maxRetries = 5
	retryDelay = time.Millisecond

	logger := log.NewTestLogger(t)
	s, err := New(logger, Config{})
	require.NoError(t, err)

	_, _, err = s.downloadURLWithRetries(ctx, ur)
	require.Error(t, err)
	assert.True(t, IsHTTPStatusError(err, http.StatusForbidden))
	// 403 is not retryable, so only 1 request should have been made.
	assert.Equal(t, int64(1), atomic.LoadInt64(&requestCount))
}

func TestDownloadURLWithRetries_RetryAfterSeconds(t *testing.T) {
	ctx := context.Background()

	var attempt int64
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&attempt, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer svr.Close()

	ur, err := url.Parse(svr.URL)
	require.NoError(t, err)

	origMaxRetries := maxRetries
	origRetryDelay := retryDelay
	t.Cleanup(func() {
		maxRetries = origMaxRetries
		retryDelay = origRetryDelay
	})
	maxRetries = 3
	retryDelay = 100 * time.Millisecond // much smaller than Retry-After=2s

	logger := log.NewTestLogger(t)
	s, err := New(logger, Config{})
	require.NoError(t, err)

	start := time.Now()
	b, _, err := s.downloadURLWithRetries(ctx, ur)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, "ok", string(b))
	// Retry-After=2 means sleep ~2s; without it, sleep would be ~100ms.
	assert.GreaterOrEqual(t, elapsed, 1900*time.Millisecond,
		"should have slept at least ~2s due to Retry-After header")
}

// streamingTestScraper builds a Scraper wired for streaming tests and a
// temp file path for the streamed body.
func streamingTestScraper(t *testing.T) (*Scraper, string) {
	t.Helper()
	logger := log.NewTestLogger(t)
	s, err := New(logger, Config{})
	require.NoError(t, err)
	return s, filepath.Join(t.TempDir(), "out.bin")
}

func TestDownloadURLStreaming_RetryThenSuccess(t *testing.T) {
	ctx := context.Background()

	var retry int
	expected := []byte("streamed ok")
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retry < maxRetries {
			retry++
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(expected)
	}))
	defer svr.Close()

	ur, err := url.Parse(svr.URL)
	require.NoError(t, err)

	origMaxRetries := maxRetries
	origRetryDelay := retryDelay
	t.Cleanup(func() {
		maxRetries = origMaxRetries
		retryDelay = origRetryDelay
	})
	maxRetries = 2
	retryDelay = time.Millisecond

	s, outPath := streamingTestScraper(t)

	_, err = s.downloadURLStreaming(ctx, ur, outPath)
	require.NoError(t, err)
	assert.Equal(t, retry, maxRetries)

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Equal(t, expected, data)
}

func TestDownloadURLStreaming_403NoRetry(t *testing.T) {
	ctx := context.Background()

	var requestCount int64
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer svr.Close()

	ur, err := url.Parse(svr.URL)
	require.NoError(t, err)

	origMaxRetries := maxRetries
	origRetryDelay := retryDelay
	t.Cleanup(func() {
		maxRetries = origMaxRetries
		retryDelay = origRetryDelay
	})
	maxRetries = 5
	retryDelay = time.Millisecond

	s, outPath := streamingTestScraper(t)

	_, err = s.downloadURLStreaming(ctx, ur, outPath)
	require.Error(t, err)
	assert.True(t, IsHTTPStatusError(err, http.StatusForbidden))
	assert.Equal(t, int64(1), atomic.LoadInt64(&requestCount), "403 must not retry")

	// Partial file must not exist after error (streaming path removes it).
	_, statErr := os.Stat(outPath)
	assert.True(t, os.IsNotExist(statErr), "no file should remain after 403")
}

func TestDownloadURLStreaming_RetryAfterSeconds(t *testing.T) {
	ctx := context.Background()

	var attempt int64
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&attempt, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer svr.Close()

	ur, err := url.Parse(svr.URL)
	require.NoError(t, err)

	origMaxRetries := maxRetries
	origRetryDelay := retryDelay
	t.Cleanup(func() {
		maxRetries = origMaxRetries
		retryDelay = origRetryDelay
	})
	maxRetries = 3
	retryDelay = 100 * time.Millisecond

	s, outPath := streamingTestScraper(t)

	start := time.Now()
	_, err = s.downloadURLStreaming(ctx, ur, outPath)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, elapsed, 1900*time.Millisecond,
		"streaming path must honor Retry-After ~2s")
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected time.Duration
	}{
		{"empty", "", 0},
		{"seconds_2", "2", 2 * time.Second},
		{"seconds_0", "0", 0},
		{"seconds_negative", "-1", 0},
		{"seconds_large_capped", "600", 5 * time.Minute},
		{"garbage", "not-a-number-or-date", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryAfter(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}

	// HTTP-date in the future
	t.Run("http_date_future", func(t *testing.T) {
		future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
		got := parseRetryAfter(future)
		// Should be roughly 30s (allow 25-35s for test timing)
		assert.Greater(t, got, 25*time.Second)
		assert.Less(t, got, 35*time.Second)
	})

	// HTTP-date in the past
	t.Run("http_date_past", func(t *testing.T) {
		past := time.Now().Add(-10 * time.Second).UTC().Format(http.TimeFormat)
		got := parseRetryAfter(past)
		assert.Equal(t, time.Duration(0), got)
	})
}
