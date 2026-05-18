package scraper

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cornelk/gotokit/log"
	"github.com/cornelk/gotokit/set"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	require.NoError(t, err)
	return u
}

// TestQueryDropDefault — without IncludeQueryInPath, query is dropped from
// both dedup key and on-disk filename (preserves prior behavior).
func TestQueryDropDefault(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org"})
	require.NoError(t, err)
	s.processed = set.New[string]()

	u1 := mustParseURL(t, "https://example.org/article?id=1")
	u2 := mustParseURL(t, "https://example.org/article?id=2")

	assert.True(t, s.shouldURLBeDownloaded(u1, 0, false))
	assert.False(t, s.shouldURLBeDownloaded(u2, 0, false),
		"default behavior: query-only difference must dedup")

	// On-disk filename: same path, query dropped.
	assert.Equal(t, s.getFilePath(u1, true), s.getFilePath(u2, true))
}

// TestQueryIncludeOptIn — with IncludeQueryInPath, URLs that differ only
// in query are tracked separately and write to distinct files.
func TestQueryIncludeOptIn(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org", IncludeQueryInPath: true})
	require.NoError(t, err)
	s.processed = set.New[string]()

	u1 := mustParseURL(t, "https://example.org/article?id=1")
	u2 := mustParseURL(t, "https://example.org/article?id=2")

	assert.True(t, s.shouldURLBeDownloaded(u1, 0, false))
	assert.True(t, s.shouldURLBeDownloaded(u2, 0, false),
		"with IncludeQueryInPath, query-only difference must NOT dedup")

	p1 := s.getFilePath(u1, true)
	p2 := s.getFilePath(u2, true)
	assert.NotEqual(t, p1, p2, "distinct queries must write to distinct files")
	assert.True(t, strings.HasSuffix(p1, ".html"))
	assert.True(t, strings.HasSuffix(p2, ".html"))
}

// TestQueryCanonicalOrder — "?b=1&a=2" and "?a=2&b=1" must canonicalize
// to the same hash so the order in HTML doesn't fragment the dedup space.
func TestQueryCanonicalOrder(t *testing.T) {
	u1 := mustParseURL(t, "https://example.org/x?b=1&a=2")
	u2 := mustParseURL(t, "https://example.org/x?a=2&b=1")
	assert.Equal(t, queryHash(u1), queryHash(u2))
	assert.Equal(t, canonicalQuery(u1), canonicalQuery(u2))
}

// TestQueryHashInsertedBeforeExtension — for a binary asset, the hash
// must sit between the base name and the extension.
func TestQueryHashInsertedBeforeExtension(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org", IncludeQueryInPath: true})
	require.NoError(t, err)

	u := mustParseURL(t, "https://example.org/app.js?v=42")
	got := s.getFilePath(u, false)
	base := filepath.Base(got)
	assert.True(t, strings.HasPrefix(base, "app-"), "filename must start with original base, got: %s", base)
	assert.True(t, strings.HasSuffix(base, ".js"), "filename must end with original extension, got: %s", base)
}

// TestQueryAbsentNoSuffix — no query string → filename unchanged even when
// the opt-in flag is set.
func TestQueryAbsentNoSuffix(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org", IncludeQueryInPath: true})
	require.NoError(t, err)

	u := mustParseURL(t, "https://example.org/about")
	got := s.getFilePath(u, true)
	assert.True(t, strings.HasSuffix(got, "about.html"), "no-query URL must keep clean filename, got: %s", got)
}

// TestCDNConcatNotTreatedAsQuery — "/path/??a.js,b.js" is folded into the
// path by other helpers; the query-hash logic must skip it so we don't
// produce a "??-hash" artifact.
func TestCDNConcatNotTreatedAsQuery(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org", IncludeQueryInPath: true})
	require.NoError(t, err)

	u := mustParseURL(t, "https://example.org/combo/??a.js,b.js")
	got := s.getFilePath(u, false)
	assert.NotContains(t, filepath.Base(got), "-",
		"CDN concat must not get a query hash appended, got: %s", got)
}
