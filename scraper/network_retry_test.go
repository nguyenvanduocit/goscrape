package scraper

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cornelk/gotokit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedRoundTripper returns the configured errors in order; once exhausted
// it returns whatever errs[len-1] was. Tracks call count via calls.
type scriptedRoundTripper struct {
	errs    []error
	resp    *http.Response
	calls   atomic.Int64
	respAt  int // index at which to return resp instead of an error; -1 = never
	respErr error
}

func (s *scriptedRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	n := s.calls.Add(1)
	idx := int(n - 1)
	if s.respAt >= 0 && idx >= s.respAt {
		return s.resp, s.respErr
	}
	if idx < len(s.errs) {
		return nil, s.errs[idx]
	}
	return nil, s.errs[len(s.errs)-1]
}

// timeoutError satisfies net.Error with Timeout()==true so the retry detection
// path can be exercised without real network I/O. net.Error still requires
// Temporary() in current Go even though it's documented as deprecated.
type timeoutError struct{}

func (timeoutError) Error() string   { return "fake timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// closeRespBody safely closes a response body if non-nil. Used in test
// helpers so the bodyclose linter is satisfied and real-world callers'
// resource hygiene gets exercised too.
func closeRespBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func newRetryScraper(t *testing.T, rt http.RoundTripper) *Scraper {
	t.Helper()
	s, err := New(log.NewNop(), Config{URL: "https://example.org"})
	require.NoError(t, err)
	s.client.Transport = rt
	return s
}

func withTinyRetryDelay(t *testing.T) {
	t.Helper()
	orig := retryDelay
	retryDelay = time.Millisecond
	t.Cleanup(func() { retryDelay = orig })
}

func withMaxRetries(t *testing.T, n int) {
	t.Helper()
	orig := maxRetries
	maxRetries = n
	t.Cleanup(func() { maxRetries = orig })
}

// TestRetryOnTimeoutThenSuccess — first two attempts time out, third
// succeeds. requestWithRetry must wait and ultimately return the response.
func TestRetryOnTimeoutThenSuccess(t *testing.T) {
	withTinyRetryDelay(t)
	withMaxRetries(t, 5)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(nil),
		Header:     http.Header{},
		Request:    &http.Request{URL: &url.URL{Scheme: "https", Host: "example.org"}},
	}
	rt := &scriptedRoundTripper{
		errs:   []error{timeoutError{}, timeoutError{}},
		resp:   resp,
		respAt: 2,
	}
	s := newRetryScraper(t, rt)

	u, _ := url.Parse("https://example.org/x")
	got, err := s.requestWithRetry(context.Background(), u)
	defer closeRespBody(t, got)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(3), rt.calls.Load(), "expected 2 retries + 1 success")
}

// TestRetryExhaustedWrapsLastError — after maxRetries+1 transient failures,
// the returned error is errExhaustedRetries with the last network error
// preserved in the chain.
func TestRetryExhaustedWrapsLastError(t *testing.T) {
	withTinyRetryDelay(t)
	withMaxRetries(t, 2)

	lastErr := &net.OpError{Op: "read", Err: syscall.ECONNRESET}
	rt := &scriptedRoundTripper{
		errs:   []error{timeoutError{}, timeoutError{}, lastErr},
		respAt: -1,
	}
	s := newRetryScraper(t, rt)

	u, _ := url.Parse("https://example.org/x")
	resp, err := s.requestWithRetry(context.Background(), u)
	defer closeRespBody(t, resp)
	require.ErrorIs(t, err, errExhaustedRetries)
	require.ErrorIs(t, err, syscall.ECONNRESET, "last underlying network error must be in chain")
}

// TestNoRetryOnContextCanceled — ctx canceled before a retry attempt must
// short-circuit without further requests.
func TestNoRetryOnContextCanceled(t *testing.T) {
	withTinyRetryDelay(t)
	withMaxRetries(t, 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rt := &scriptedRoundTripper{
		errs:   []error{context.Canceled},
		respAt: -1,
	}
	s := newRetryScraper(t, rt)

	u, _ := url.Parse("https://example.org/x")
	resp, err := s.requestWithRetry(ctx, u)
	defer closeRespBody(t, resp)
	require.Error(t, err)
	require.NotErrorIs(t, err, errExhaustedRetries, "context cancel must not be retried")
	assert.LessOrEqual(t, rt.calls.Load(), int64(1), "no retries on canceled context")
}

// TestNoRetryOnSSRFBlock — SSRF block is permanent; must return immediately.
func TestNoRetryOnSSRFBlock(t *testing.T) {
	withTinyRetryDelay(t)
	withMaxRetries(t, 5)

	rt := &scriptedRoundTripper{
		errs:   []error{ErrBlockedPrivateAddress},
		respAt: -1,
	}
	s := newRetryScraper(t, rt)

	u, _ := url.Parse("https://example.org/x")
	resp, err := s.requestWithRetry(context.Background(), u)
	defer closeRespBody(t, resp)
	require.ErrorIs(t, err, ErrBlockedPrivateAddress)
	require.NotErrorIs(t, err, errExhaustedRetries)
	assert.Equal(t, int64(1), rt.calls.Load(), "SSRF block must not retry")
}

// TestNoRetryOnDNSNotFound — DNS NXDOMAIN is permanent.
func TestNoRetryOnDNSNotFound(t *testing.T) {
	withTinyRetryDelay(t)
	withMaxRetries(t, 5)

	dnsErr := &net.DNSError{Name: "no.such.host", IsNotFound: true}
	rt := &scriptedRoundTripper{errs: []error{dnsErr}, respAt: -1}
	s := newRetryScraper(t, rt)

	u, _ := url.Parse("https://no.such.host/x")
	resp, err := s.requestWithRetry(context.Background(), u)
	defer closeRespBody(t, resp)
	require.Error(t, err)
	require.NotErrorIs(t, err, errExhaustedRetries, "NXDOMAIN must not retry")
	assert.Equal(t, int64(1), rt.calls.Load())
}

// TestRetryOnDNSTimeout — DNS timeout is transient.
func TestRetryOnDNSTimeout(t *testing.T) {
	withTinyRetryDelay(t)
	withMaxRetries(t, 2)

	dnsErr := &net.DNSError{Name: "slow.host", IsTimeout: true}
	rt := &scriptedRoundTripper{errs: []error{dnsErr, dnsErr, dnsErr}, respAt: -1}
	s := newRetryScraper(t, rt)

	u, _ := url.Parse("https://slow.host/x")
	resp, err := s.requestWithRetry(context.Background(), u)
	defer closeRespBody(t, resp)
	require.ErrorIs(t, err, errExhaustedRetries)
	assert.Equal(t, int64(3), rt.calls.Load(), "DNS timeout must be retried")
}

// clientTimeoutError mimics the error chain http.Client.Timeout produces:
// the underlying error carries context.DeadlineExceeded (because the client
// cancels the request context when its per-call timeout fires) AND its
// Timeout() reports true. requestWithRetry must still retry this when the
// caller's context itself has not expired.
type clientTimeoutError struct{}

func (clientTimeoutError) Error() string   { return "Client.Timeout exceeded while awaiting headers" }
func (clientTimeoutError) Timeout() bool   { return true }
func (clientTimeoutError) Temporary() bool { return true }
func (clientTimeoutError) Unwrap() error   { return context.DeadlineExceeded }

// TestRetryOnClientTimeoutWithLiveCtx — http.Client.Timeout produces an
// error whose chain contains context.DeadlineExceeded even when the
// caller's context is still live. requestWithRetry must retry on this
// (regression guard for Codex BLOCK in Task #8).
func TestRetryOnClientTimeoutWithLiveCtx(t *testing.T) {
	withTinyRetryDelay(t)
	withMaxRetries(t, 2)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(nil),
		Header:     http.Header{},
		Request:    &http.Request{URL: &url.URL{Scheme: "https", Host: "example.org"}},
	}
	rt := &scriptedRoundTripper{
		errs:   []error{clientTimeoutError{}, clientTimeoutError{}},
		resp:   resp,
		respAt: 2,
	}
	s := newRetryScraper(t, rt)

	u, _ := url.Parse("https://example.org/x")
	got, err := s.requestWithRetry(context.Background(), u)
	defer closeRespBody(t, got)
	require.NoError(t, err, "client timeout with live caller ctx must be retried")
	assert.Equal(t, int64(3), rt.calls.Load())
}

// TestIsRetryableNetworkError_TableDriven — direct unit test for the
// classifier covering each branch.
func TestIsRetryableNetworkError_TableDriven(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// Caller-context cancellation is handled at the requestWithRetry
		// call site (via ctx.Err()), not by this classifier:
		//   - context.Canceled has no Timeout() method, falls through to false.
		//   - context.DeadlineExceeded implements net.Error.Timeout()==true,
		//     so it's classified as a retryable network timeout (matching
		//     http.Client.Timeout behavior). The call-site ctx.Err() check
		//     still prevents real caller aborts from being retried.
		{"raw_context_canceled", context.Canceled, false},
		{"raw_context_deadline_treated_as_timeout", context.DeadlineExceeded, true},
		{"ssrf_block", ErrBlockedPrivateAddress, false},
		{"dns_nxdomain", &net.DNSError{IsNotFound: true}, false},
		{"dns_timeout", &net.DNSError{IsTimeout: true}, true},
		{"dns_temporary", &net.DNSError{IsTemporary: true}, true},
		{"net_timeout", timeoutError{}, true},
		{"econn_reset", syscall.ECONNRESET, true},
		{"econn_refused", syscall.ECONNREFUSED, true},
		{"etimedout", syscall.ETIMEDOUT, true},
		{"ehostunreach", syscall.EHOSTUNREACH, true},
		{"enetunreach", syscall.ENETUNREACH, true},
		{"epipe", syscall.EPIPE, true},
		{"eof", io.EOF, true},
		{"unexpected_eof", io.ErrUnexpectedEOF, true},
		{"unknown_error", errors.New("something unknown"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isRetryableNetworkError(tt.err))
		})
	}
}
