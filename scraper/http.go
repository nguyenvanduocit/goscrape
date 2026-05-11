package scraper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

func (s *Scraper) downloadURLWithRetries(ctx context.Context, u *url.URL) ([]byte, *url.URL, error) {
	// Apply rate limiting if configured
	if s.rateLimiter != nil {
		if err := s.rateLimiter.Wait(ctx); err != nil {
			return nil, nil, fmt.Errorf("rate limiter: %w", err)
		}
	}

	// Apply delay if configured
	if s.config.Delay > 0 {
		if err := app.Sleep(ctx, time.Duration(s.config.Delay)*time.Millisecond); err != nil {
			return nil, nil, fmt.Errorf("delay: %w", err)
		}
	}

	var resp *http.Response

	for attempt := range maxRetries + 1 {
		var err error
		resp, err = s.downloadURL(ctx, u)
		if err != nil {
			return nil, nil, err
		}

		if !retryableStatusCodes[resp.StatusCode] {
			break
		}

		retryAfterHeader := resp.Header.Get("Retry-After")
		_ = resp.Body.Close()

		if attempt == maxRetries {
			return nil, nil, fmt.Errorf("%w for URL %s", errExhaustedRetries, u)
		}

		s.logger.Warn("Retryable HTTP status. Retrying",
			log.Int("status", resp.StatusCode),
			log.Int("attempt", attempt+1),
			log.Int("max", maxRetries),
			log.String("url", u.String()))

		sleep := time.Duration(attempt+1) * retryDelay
		if ra := parseRetryAfter(retryAfterHeader); ra > 0 {
			sleep = ra
		}
		if err := app.Sleep(ctx, sleep); err != nil {
			return nil, nil, fmt.Errorf("sleeping between retries: %w", err)
		}
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			s.logger.Error("Closing HTTP Request body failed",
				log.String("url", u.String()),
				log.Err(err))
		}
	}()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusForbidden {
			s.logger.Warn("HTTP 403 Forbidden, skipping",
				log.String("url", u.String()))
		}
		return nil, nil, &HTTPStatusError{StatusCode: resp.StatusCode, URL: u.String()}
	}

	buf := &bytes.Buffer{}
	if _, err := io.Copy(buf, io.LimitReader(resp.Body, maxResponseBodySize)); err != nil {
		return nil, nil, fmt.Errorf("reading HTTP request body: %w", err)
	}
	return buf.Bytes(), resp.Request.URL, nil
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
