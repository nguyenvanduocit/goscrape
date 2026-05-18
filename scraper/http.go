package scraper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cornelk/gotokit/app"
	"github.com/cornelk/gotokit/log"
)

var (
	maxRetries = 10
	retryDelay = 1500 * time.Millisecond

	errExhaustedRetries = errors.New("exhausted retries")

	// streamingMaxBytes is the maximum number of bytes streamed to disk for a single
	// asset in the streaming path. Large enough for videos; prevents disk exhaustion.
	streamingMaxBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GB

	// retryableStatusCodes are HTTP status codes that should trigger a retry.
	// Note: 403 Forbidden is intentionally excluded — it indicates a permanent
	// access denial (e.g. auth-gated pages) and retrying wastes time and risks IP bans.
	retryableStatusCodes = map[int]bool{
		http.StatusTooManyRequests:     true, // 429
		http.StatusInternalServerError: true, // 500
		http.StatusBadGateway:          true, // 502
		http.StatusServiceUnavailable:  true, // 503
		http.StatusGatewayTimeout:      true, // 504
	}

	// maxResponseBodySize is the maximum response body size (bytes) to read into memory.
	maxResponseBodySize int64 = 100 * 1024 * 1024
)

// HTTPStatusError is returned when an HTTP request returns a non-200 status code.
type HTTPStatusError struct {
	StatusCode int
	URL        string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("unexpected HTTP request status code %d", e.StatusCode)
}

// IsHTTPStatusError checks if an error is an HTTPStatusError with a specific status code.
func IsHTTPStatusError(err error, statusCode int) bool {
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == statusCode
	}
	return false
}

// parseRetryAfter parses the Retry-After header value into a duration.
// It handles both delay-seconds ("120") and HTTP-date formats.
// Returns 0 if the header is empty or unparseable.
// The result is clamped to [1s, 5m].
func parseRetryAfter(headerVal string) time.Duration {
	if headerVal == "" {
		return 0
	}

	// Try as seconds integer first.
	if n, err := strconv.Atoi(headerVal); err == nil {
		if n <= 0 {
			return 0
		}
		d := time.Duration(n) * time.Second
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
		return d
	}

	// Try as HTTP-date.
	if t, err := http.ParseTime(headerVal); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 0
		}
		if d < time.Second {
			d = time.Second
		}
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
		return d
	}

	return 0
}

func (s *Scraper) downloadURL(ctx context.Context, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}

	req.Header.Set("User-Agent", s.config.UserAgent)
	if s.auth != "" {
		req.Header.Set("Authorization", s.auth)
	}

	for key, values := range s.config.Header {
		for _, value := range values {
			req.Header.Set(key, value)
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing HTTP request: %w", err)
	}

	return resp, nil
}

// requestWithRetry handles rate limiting, delay, retry loop, and Retry-After
// parsing. It returns the first non-retryable HTTP response (success OR
// permanent error like 403) and transfers body ownership to the caller —
// the caller MUST close resp.Body and is responsible for status checking
// and body consumption.
func (s *Scraper) requestWithRetry(ctx context.Context, u *url.URL) (*http.Response, error) {
	if err := s.hostLimiter.Wait(ctx, u.Host); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}
	if s.config.Delay > 0 {
		if err := app.Sleep(ctx, time.Duration(s.config.Delay)*time.Millisecond); err != nil {
			return nil, fmt.Errorf("delay: %w", err)
		}
	}

	for attempt := range maxRetries + 1 {
		resp, err := s.downloadURL(ctx, u)
		if err != nil {
			return nil, err
		}
		if !retryableStatusCodes[resp.StatusCode] {
			return resp, nil
		}

		retryAfterHeader := resp.Header.Get("Retry-After")
		_ = resp.Body.Close()

		if attempt == maxRetries {
			return nil, fmt.Errorf("%w for URL %s", errExhaustedRetries, u)
		}

		s.logger.Warn("Retryable HTTP status. Retrying",
			log.Int("status", resp.StatusCode),
			log.Int("attempt", attempt+1),
			log.Int("max", maxRetries),
			log.String("url", u.String()))

		sleep := time.Duration(attempt+1) * retryDelay
		sleep += time.Duration(rand.Int64N(int64(retryDelay))) // 0 to retryDelay jitter
		if ra := parseRetryAfter(retryAfterHeader); ra > 0 {
			sleep = ra // honor server signal verbatim, no jitter
		}
		if err := app.Sleep(ctx, sleep); err != nil {
			return nil, fmt.Errorf("sleeping between retries: %w", err)
		}
	}
	// Unreachable: the loop either returns on success or on retry exhaustion.
	return nil, fmt.Errorf("%w for URL %s", errExhaustedRetries, u)
}

// checkResponseStatus returns an HTTPStatusError for non-200 responses,
// logging a warning for 403 (typically permanent denial).
func (s *Scraper) checkResponseStatus(resp *http.Response, u *url.URL) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusForbidden {
		s.logger.Warn("HTTP 403 Forbidden, skipping", log.String("url", u.String()))
	}
	return &HTTPStatusError{StatusCode: resp.StatusCode, URL: u.String()}
}

// closeResponseBody closes resp.Body and logs any close error.
func (s *Scraper) closeResponseBody(resp *http.Response, u *url.URL) {
	if err := resp.Body.Close(); err != nil {
		s.logger.Error("Closing HTTP request body failed",
			log.String("url", u.String()),
			log.Err(err))
	}
}

func (s *Scraper) downloadURLWithRetries(ctx context.Context, u *url.URL) ([]byte, *url.URL, error) {
	resp, err := s.requestWithRetry(ctx, u)
	if err != nil {
		return nil, nil, err
	}
	defer s.closeResponseBody(resp, u)

	if err := s.checkResponseStatus(resp, u); err != nil {
		return nil, nil, err
	}

	buf := &bytes.Buffer{}
	n, err := io.Copy(buf, io.LimitReader(resp.Body, maxResponseBodySize+1)) // +1 to detect overflow
	if err != nil {
		return nil, nil, fmt.Errorf("reading HTTP request body: %w", err)
	}
	if n > maxResponseBodySize {
		return nil, nil, fmt.Errorf("response body exceeded %d bytes for URL %s", maxResponseBodySize, u.String())
	}
	return buf.Bytes(), resp.Request.URL, nil
}

// downloadURLStreaming performs the same retry/rate-limit flow as
// downloadURLWithRetries, but streams the response body directly to filePath
// instead of buffering in memory. Use for large assets with no processor.
// Caller must ensure the parent directory exists before calling.
// On any error after os.Create, the partial file is removed.
func (s *Scraper) downloadURLStreaming(ctx context.Context, u *url.URL, filePath string) (*url.URL, error) {
	resp, err := s.requestWithRetry(ctx, u)
	if err != nil {
		return nil, err
	}
	defer s.closeResponseBody(resp, u)

	if err := s.checkResponseStatus(resp, u); err != nil {
		return nil, err
	}

	f, err := os.Create(filePath)
	if err != nil {
		return nil, fmt.Errorf("creating output file '%s': %w", filePath, err)
	}

	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, streamingMaxBytes+1))
	closeErr := f.Close()

	if copyErr != nil || n > streamingMaxBytes || closeErr != nil {
		_ = os.Remove(filePath) // remove partial file on any failure
		switch {
		case copyErr != nil:
			return nil, fmt.Errorf("streaming response body to file: %w", copyErr)
		case n > streamingMaxBytes:
			return nil, fmt.Errorf("response body exceeded %d bytes for URL %s", streamingMaxBytes, u.String())
		default:
			return nil, fmt.Errorf("closing streamed file: %w", closeErr)
		}
	}

	return resp.Request.URL, nil
}

// Headers converts a slice of strings to a http.Header.
func Headers(headers []string) http.Header {
	h := http.Header{}
	for _, header := range headers {
		sl := strings.SplitN(header, ":", 2)
		if len(sl) == 2 {
			h.Set(sl[0], sl[1])
		}
	}
	return h
}
