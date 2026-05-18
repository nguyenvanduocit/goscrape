package scraper

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cornelk/gotokit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCasePathCollision_FirstWinsSecondSuffixed — first URL to claim a
// lowercased path keeps the original casing; a later URL whose path
// differs only by case gets a deterministic hash suffix.
func TestCasePathCollision_FirstWinsSecondSuffixed(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org"})
	require.NoError(t, err)

	u1 := mustParseURL(t, "https://example.org/Logo.png")
	u2 := mustParseURL(t, "https://example.org/logo.png")

	p1 := s.getFilePath(u1, false)
	p2 := s.getFilePath(u2, false)

	assert.True(t, strings.HasSuffix(p1, "Logo.png"), "first URL keeps original casing, got: %s", p1)
	assert.NotEqual(t, p1, p2, "case-only collision must produce distinct paths")
	assert.True(t, strings.HasSuffix(p2, ".png"), "collision-suffixed file keeps original extension, got: %s", p2)
	assert.NotEqual(t, strings.ToLower(filepath.Base(p1)), strings.ToLower(filepath.Base(p2)),
		"the lowercased basenames must now differ")
}

// TestCasePathCollision_RepeatedURLStable — calling getFilePath multiple
// times with the same URL returns the same path (first call wins, later
// calls don't keep mutating).
func TestCasePathCollision_RepeatedURLStable(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org"})
	require.NoError(t, err)

	u1 := mustParseURL(t, "https://example.org/Logo.png")
	u2 := mustParseURL(t, "https://example.org/logo.png")

	p1a := s.getFilePath(u1, false)
	p2a := s.getFilePath(u2, false)
	p1b := s.getFilePath(u1, false)
	p2b := s.getFilePath(u2, false)

	assert.Equal(t, p1a, p1b, "repeat for same URL must return same path")
	assert.Equal(t, p2a, p2b, "repeat for same URL must return same path")
}

// TestNoCaseCollisionWhenSameFilepath — two URLs that legitimately produce
// the SAME filepath (e.g. query-drop default) must NOT be treated as a
// case collision and rewritten.
func TestNoCaseCollisionWhenSameFilepath(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org"})
	require.NoError(t, err)

	u1 := mustParseURL(t, "https://example.org/article?id=1")
	u2 := mustParseURL(t, "https://example.org/article?id=2")

	assert.Equal(t, s.getFilePath(u1, true), s.getFilePath(u2, true),
		"same target filepath is not a case collision and must overwrite as designed")
}

// TestCasePathCollision_HTMLPage — collision detection works for page paths
// too, not just binary assets.
func TestCasePathCollision_HTMLPage(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org"})
	require.NoError(t, err)

	u1 := mustParseURL(t, "https://example.org/About")
	u2 := mustParseURL(t, "https://example.org/about")

	p1 := s.getFilePath(u1, true)
	p2 := s.getFilePath(u2, true)
	assert.NotEqual(t, p1, p2, "HTML page case collision must produce distinct paths")
}

// TestCasePathCollision_LiteralMatchesSynthetic — guards against the
// secondary-collision scenario Codex flagged: if /Logo.png and /logo.png
// have already disambiguated to logo-<hash>.png, a literal third URL
// /logo-<hash>.png must not silently clobber the synthetic slot.
func TestCasePathCollision_LiteralMatchesSynthetic(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org"})
	require.NoError(t, err)

	u1 := mustParseURL(t, "https://example.org/Logo.png")
	u2 := mustParseURL(t, "https://example.org/logo.png")
	p1 := s.getFilePath(u1, false)
	p2 := s.getFilePath(u2, false)
	require.NotEqual(t, p1, p2)

	// The synthetic filename for u2 looks like "logo-<hash>.png". Construct a
	// literal URL that points at that exact filename and demand it gets its
	// OWN slot rather than overwriting u2's claim.
	syntheticBase := filepath.Base(p2)
	stem := strings.TrimSuffix(syntheticBase, filepath.Ext(syntheticBase))
	literal := mustParseURL(t, "https://example.org/"+stem+".png")
	p3 := s.getFilePath(literal, false)
	assert.NotEqual(t, p2, p3,
		"literal URL whose path matches a previously-synthesized name must get its own slot")
}

// TestCasePathCollision_Concurrent — under race detector, concurrent
// getFilePath calls on mixed-case URLs must produce a deterministic
// partition: exactly one URL wins each lowercased path.
func TestCasePathCollision_Concurrent(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org"})
	require.NoError(t, err)

	const goroutines = 32
	u1 := mustParseURL(t, "https://example.org/Logo.png")
	u2 := mustParseURL(t, "https://example.org/logo.png")

	var p1, p2 atomic.Value
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		u := u1
		if i%2 == 1 {
			u = u2
		}
		go func() {
			defer wg.Done()
			<-start
			got := s.getFilePath(u, false)
			if u == u1 {
				p1.Store(got)
			} else {
				p2.Store(got)
			}
		}()
	}
	close(start)
	wg.Wait()

	v1, _ := p1.Load().(string)
	v2, _ := p2.Load().(string)
	require.NotEmpty(t, v1)
	require.NotEmpty(t, v2)
	assert.NotEqual(t, v1, v2, "concurrent collision must still produce distinct paths")
}
