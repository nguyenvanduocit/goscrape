package scraper

import (
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cornelk/gotokit/log"
	"github.com/cornelk/gotokit/set"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/temoto/robotstxt"
)

// newDedupScraper builds a minimal Scraper for shouldURLBeDownloaded tests.
func newDedupScraper(t *testing.T, cfg Config) *Scraper {
	t.Helper()
	s, err := New(log.NewNop(), cfg)
	require.NoError(t, err)
	s.processed = set.New[string]()
	return s
}

// TestRejectedURLIsNotMarked — depth/include/exclude/default-exclude reject
// must NOT mark the URL as processed, so a later eligible discovery still
// passes (no poisoning).
func TestRejectedURLIsNotMarked(t *testing.T) {
	t.Run("depth_reject_then_eligible", func(t *testing.T) {
		s := newDedupScraper(t, Config{URL: "https://example.org", MaxDepth: 2})
		u, _ := url.Parse("https://example.org/article")

		// At MaxDepth, page is rejected.
		assert.False(t, s.shouldURLBeDownloaded(u, 2, false))
		assert.False(t, s.processed.Contains(s.processedKey(u)),
			"depth-rejected URL must NOT be marked")

		// Same URL discovered at lower depth is eligible and passes.
		assert.True(t, s.shouldURLBeDownloaded(u, 1, false))
		assert.True(t, s.processed.Contains(s.processedKey(u)))
	})

	t.Run("exclude_reject_then_disabled", func(t *testing.T) {
		s := newDedupScraper(t, Config{URL: "https://example.org"})
		s.excludes = []*regexp.Regexp{regexp.MustCompile(`/private/`)}
		u, _ := url.Parse("https://example.org/private/page")

		assert.False(t, s.shouldURLBeDownloaded(u, 0, false))
		assert.False(t, s.processed.Contains(s.processedKey(u)),
			"exclude-rejected URL must NOT be marked")

		// If excludes cleared, same URL passes (no residue from earlier reject).
		s.excludes = nil
		assert.True(t, s.shouldURLBeDownloaded(u, 0, false))
	})

	t.Run("default_exclude_reject_not_marked", func(t *testing.T) {
		s := newDedupScraper(t, Config{URL: "https://example.org"})
		u, _ := url.Parse("https://example.org/login")
		assert.False(t, s.shouldURLBeDownloaded(u, 0, false))
		assert.False(t, s.processed.Contains(s.processedKey(u)),
			"default-excluded URL must NOT be marked")
	})

	t.Run("robots_reject_not_marked", func(t *testing.T) {
		s := newDedupScraper(t, Config{URL: "https://example.org"})
		robots, err := robotstxt.FromBytes([]byte("User-agent: *\nDisallow: /private/\n"))
		require.NoError(t, err)
		s.robotsData = robots

		u, _ := url.Parse("https://example.org/private/secret")
		assert.False(t, s.shouldURLBeDownloaded(u, 0, false))
		assert.False(t, s.processed.Contains(s.processedKey(u)),
			"robots-blocked URL must NOT be marked")

		// If robots rule is dropped, the same URL must pass on next discovery.
		s.robotsData = nil
		assert.True(t, s.shouldURLBeDownloaded(u, 0, false))
	})
}

// TestExternalPageThenAllowedCDNAsset — Codex finding: external URL seen
// first as a page (rejected as external) used to poison the dedup set,
// so when the SAME URL was later discovered as an allowed-CDN asset it
// would be silently skipped.
func TestExternalPageThenAllowedCDNAsset(t *testing.T) {
	s := newDedupScraper(t, Config{
		URL:                   "https://example.org",
		SkipExternalResources: true,
		AllowCDN:              []string{"cdn.example.com"},
	})
	s.allowedCDN = set.NewFromSlice([]string{"cdn.example.com"})

	u, _ := url.Parse("https://cdn.example.com/lib.js")

	// First discovered as a page → external page reject, NOT marked.
	assert.False(t, s.shouldURLBeDownloaded(u, 1, false))
	assert.False(t, s.processed.Contains(s.processedKey(u)),
		"external page reject must NOT mark URL")

	// Re-discovered as an allowed CDN asset → must pass.
	assert.True(t, s.shouldURLBeDownloaded(u, 1, true),
		"allowed CDN asset must pass even after earlier page reject")
}

// TestEligibleURLDeduplicatesOnSecondCall — same URL twice as page: first
// passes and marks, second returns false.
func TestEligibleURLDeduplicatesOnSecondCall(t *testing.T) {
	s := newDedupScraper(t, Config{URL: "https://example.org"})
	u, _ := url.Parse("https://example.org/page")

	assert.True(t, s.shouldURLBeDownloaded(u, 1, false))
	assert.False(t, s.shouldURLBeDownloaded(u, 1, false),
		"second call for same URL must be deduplicated")
}

// TestConcurrentEligibleURL — N goroutines race on the same URL; exactly
// one wins the atomic check-and-mark.
func TestConcurrentEligibleURL(t *testing.T) {
	const goroutines = 32
	s := newDedupScraper(t, Config{URL: "https://example.org"})
	u, _ := url.Parse("https://example.org/race")

	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.shouldURLBeDownloaded(u, 1, false) {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int64(1), atomic.LoadInt64(&wins),
		"exactly one goroutine must win the atomic mark")
}

// TestUnmarkAllowsRetry — for asset failures already covered by Task #1,
// confirm the post-accept rollback still works after the refactor.
func TestUnmarkAllowsRetry(t *testing.T) {
	s := newDedupScraper(t, Config{URL: "https://example.org"})
	u, _ := url.Parse("https://example.org/asset.css")

	assert.True(t, s.shouldURLBeDownloaded(u, 0, true))
	s.unmarkURLProcessed(u)
	assert.False(t, s.processed.Contains(s.processedKey(u)))
	assert.True(t, s.shouldURLBeDownloaded(u, 0, true),
		"after unmark, same URL must be eligible again")
}
