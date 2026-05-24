package scraper

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/cornelk/gotokit/log"
	"github.com/cornelk/gotokit/set"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestScraper(t *testing.T, startURL string, urls map[string][]byte) *Scraper {
	t.Helper()

	logger := log.NewTestLogger(t)
	cfg := Config{
		URL: startURL,
	}
	scraper, err := New(logger, cfg)
	require.NoError(t, err)
	require.NotNil(t, scraper)

	scraper.dirCreator = func(_ string) error {
		return nil
	}
	scraper.fileWriter = func(_ string, _ []byte) error {
		return nil
	}
	scraper.fileExistenceCheck = func(_ string) bool {
		return false
	}
	scraper.httpDownloader = func(_ context.Context, url *url.URL) ([]byte, *url.URL, error) {
		ur := url.String()
		b, ok := urls[ur]
		if ok {
			return b, url, nil
		}
		return nil, nil, fmt.Errorf("url '%s' not found in test data", ur)
	}

	return scraper
}

func TestScraperLinks(t *testing.T) {
	indexPage := []byte(`
<html>
<head>
<link href=' https://example.org/style.css#fragment' rel='stylesheet' type='text/css'>
</head>
<body>
<a href="https://example.org/page2">Example</a>
</body>
</html>
`)

	page2 := []byte(`
<html>
<body>

<!--link to index with fragment-->
<a href="/#fragment">a</a>
<!--link to page with fragment-->
<a href="/sub/#fragment">a</a>

</body>
</html>
`)

	css := []byte(``)

	startURL := "https://example.org/#fragment" // start page with fragment
	urls := map[string][]byte{
		testStartURL:                    indexPage,
		"https://example.org/page2":     page2,
		"https://example.org/sub/":      indexPage,
		"https://example.org/style.css": css,
	}

	scraper := newTestScraper(t, startURL, urls)
	require.NotNil(t, scraper)

	ctx := context.Background()
	err := scraper.Start(ctx)
	require.NoError(t, err)

	expectedProcessed := set.NewFromSlice([]string{
		"/",
		"/page2",
		"/sub",
		"/style.css",
	})
	assert.Equal(t, expectedProcessed, scraper.processed)
}

// TestNoFollow_DownloadsAssetsButNotLinks verifies the allowlist behaviour:
// with NoFollow the start page and its assets (CSS, image) are downloaded, but
// <a> hyperlink targets are never queued, so the crawl does not wander.
func TestNoFollow_DownloadsAssetsButNotLinks(t *testing.T) {
	indexPage := []byte(`
<html>
<head><link href="https://example.org/style.css" rel="stylesheet" type="text/css"></head>
<body>
<img src="https://example.org/pic.png">
<a href="https://example.org/page2">link</a>
</body>
</html>
`)

	startURL := testStartURL
	urls := map[string][]byte{
		testStartURL:                    indexPage,
		"https://example.org/style.css": []byte(``),
		"https://example.org/pic.png":   []byte(``),
		"https://example.org/page2":     []byte(`<html><body>page2</body></html>`),
	}

	logger := log.NewTestLogger(t)
	cfg := Config{URL: startURL, NoFollow: true}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	s.dirCreator = func(_ string) error { return nil }
	s.fileWriter = func(_ string, _ []byte) error { return nil }
	s.fileExistenceCheck = func(_ string) bool { return false }
	s.httpDownloader = func(_ context.Context, u *url.URL) ([]byte, *url.URL, error) {
		ur := u.String()
		if b, ok := urls[ur]; ok {
			return b, u, nil
		}
		return nil, nil, fmt.Errorf("url '%s' not found in test data", ur)
	}

	require.NoError(t, s.Start(context.Background()))

	// Start page and its page assets are fetched so the page renders offline.
	assert.True(t, s.processed.Contains("/"), "start page should be processed")
	assert.True(t, s.processed.Contains("/style.css"), "CSS asset should be downloaded")
	assert.True(t, s.processed.Contains("/pic.png"), "image asset should be downloaded")
	// The hyperlink target must NOT be followed.
	assert.False(t, s.processed.Contains("/page2"), "no-follow must not queue hyperlink targets")
}

func TestScraperAttributes(t *testing.T) {
	indexPage := []byte(`
<html>
<head>
</head>

<body background="bg.gif">

<!--embedded image-->
<img src='data:image/gif;base64,R0lGODlhAQABAAD/ACwAAAAAAQABAAACADs%3D=' />

</body>
</html>
`)
	empty := []byte(``)

	startURL := testStartURL
	urls := map[string][]byte{
		testStartURL:                 indexPage,
		"https://example.org/bg.gif": empty,
	}

	scraper := newTestScraper(t, startURL, urls)
	require.NotNil(t, scraper)

	ctx := context.Background()
	err := scraper.Start(ctx)
	require.NoError(t, err)

	expectedProcessed := set.NewFromSlice([]string{
		"/",
		"/bg.gif",
	})
	assert.Equal(t, expectedProcessed, scraper.processed)
}

func TestScraperInternalCss(t *testing.T) {
	indexPage := []byte(`
<html>
<head>
<style>
h1 {
  background-image: url('https://example.org/background.jpg');
}
h2 {
  background-image: url('/img/bg.jpg');
}
h3 {
  background-image: url(bg3.jpg);
}
</style>
</head>
<body>
</body>
</html>
`)
	empty := []byte(``)

	domain := "example.org"
	file1Reference := "background.jpg"
	file2Reference := "img/bg.jpg"
	file3Reference := "bg3.jpg"
	fullURL := "https://" + domain

	urls := map[string][]byte{
		fullURL + "/":                  indexPage,
		fullURL + "/" + file1Reference: empty,
		fullURL + "/" + file2Reference: empty,
		fullURL + "/" + file3Reference: empty,
	}

	scraper := newTestScraper(t, fullURL+"/", urls)
	require.NotNil(t, scraper)

	files := map[string][]byte{}
	scraper.fileWriter = func(filePath string, data []byte) error {
		files[filePath] = data
		return nil
	}

	ctx := context.Background()
	err := scraper.Start(ctx)
	require.NoError(t, err)

	expectedProcessed := set.NewFromSlice([]string{
		"/",
		"/" + file1Reference,
		"/" + file2Reference,
		"/" + file3Reference,
	})
	require.Equal(t, expectedProcessed, scraper.processed)

	ref := domain + "/index.html"
	content := string(files[ref])
	assert.Contains(t, content, "url('"+file1Reference+"')")
	assert.Contains(t, content, "url('"+file2Reference+"')")
	assert.Contains(t, content, "url("+file3Reference+")")
}

func TestStart_403OnStartPage_ReturnsError(t *testing.T) {
	logger := log.NewTestLogger(t)
	cfg := Config{
		URL: testStartURL,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	s.dirCreator = func(_ string) error { return nil }
	s.fileWriter = func(_ string, _ []byte) error { return nil }
	s.fileExistenceCheck = func(_ string) bool { return false }
	s.httpDownloader = func(_ context.Context, u *url.URL) ([]byte, *url.URL, error) {
		return nil, nil, &HTTPStatusError{StatusCode: 403, URL: u.String()}
	}

	err = s.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403 Forbidden")
}

func TestProcessURL_403OnSubpage_ContinuesCrawling(t *testing.T) {
	indexPage := []byte(`
<html><body>
<a href="https://example.org/secret">secret</a>
<a href="https://example.org/public">public</a>
</body></html>
`)
	publicPage := []byte(`<html><body>public content</body></html>`)

	startURL := testStartURL
	urls := map[string][]byte{
		testStartURL:                 indexPage,
		"https://example.org/public": publicPage,
	}

	s := newTestScraper(t, startURL, urls)
	// Override downloader to return 403 for /secret
	origDownloader := s.httpDownloader
	s.httpDownloader = func(ctx context.Context, u *url.URL) ([]byte, *url.URL, error) {
		if u.Path == "/secret" {
			return nil, nil, &HTTPStatusError{StatusCode: 403, URL: u.String()}
		}
		return origDownloader(ctx, u)
	}

	err := s.Start(context.Background())
	require.NoError(t, err, "403 on subpage should not abort the crawl")
	assert.True(t, s.processed.Contains("/public"))
}

func TestProcessURL_SkipExisting_FileExists_SkipsDownload(t *testing.T) {
	indexPage := []byte(`
<html><body>
<a href="https://example.org/child1">child1</a>
<a href="https://example.org/child2">child2</a>
</body></html>
`)
	startURL := testStartURL

	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:          startURL,
		SkipExisting: true,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	// Track HTTP download calls
	var downloadCount int
	s.httpDownloader = func(_ context.Context, u *url.URL) ([]byte, *url.URL, error) {
		downloadCount++
		if u.String() == startURL {
			return indexPage, u, nil
		}
		return nil, nil, fmt.Errorf("unexpected download: %s", u.String())
	}
	s.dirCreator = func(_ string) error { return nil }
	s.fileWriter = func(_ string, _ []byte) error { return nil }

	// Simulate that the file for the start page already exists
	startFilePath := s.getFilePath(s.URL, true)
	s.fileExistenceCheck = func(filePath string) bool {
		return filePath == startFilePath
	}

	err = s.Start(context.Background())
	require.NoError(t, err)

	// Start page file exists → should NOT have been downloaded
	assert.Equal(t, 0, downloadCount, "no HTTP downloads should occur when file exists")
	// Children should NOT have been processed (URLs are rewritten in cached files)
	assert.False(t, s.processed.Contains("/child1"), "no children should be processed from skipped pages")
	assert.False(t, s.processed.Contains("/child2"), "no children should be processed from skipped pages")
}

func TestProcessURL_SkipExisting_FileMissing_FallsThrough(t *testing.T) {
	indexPage := []byte(`
<html><body>
<a href="https://example.org/child1">child1</a>
</body></html>
`)
	child1Page := []byte(`<html><body>child content</body></html>`)
	startURL := testStartURL
	urls := map[string][]byte{
		testStartURL:                 indexPage,
		"https://example.org/child1": child1Page,
	}

	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:          startURL,
		SkipExisting: true,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	var downloadCount int
	s.httpDownloader = func(_ context.Context, u *url.URL) ([]byte, *url.URL, error) {
		downloadCount++
		ur := u.String()
		b, ok := urls[ur]
		if ok {
			return b, u, nil
		}
		return nil, nil, fmt.Errorf("url '%s' not found", ur)
	}
	s.dirCreator = func(_ string) error { return nil }
	s.fileWriter = func(_ string, _ []byte) error { return nil }
	// File does NOT exist on disk → should fall through to download
	s.fileExistenceCheck = func(_ string) bool { return false }

	err = s.Start(context.Background())
	require.NoError(t, err)

	// Both start page and child1 should have been downloaded
	assert.Equal(t, 2, downloadCount, "both pages should be downloaded when files don't exist")
	assert.True(t, s.processed.Contains("/child1"))
}

func TestProcessURL_SkipExistingMarkdown_NoChildren(t *testing.T) {
	startURL := testStartURL

	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:          startURL,
		Markdown:     true,
		SkipExisting: true,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	var downloadCount int
	s.httpDownloader = func(_ context.Context, _ *url.URL) ([]byte, *url.URL, error) {
		downloadCount++
		return nil, nil, errors.New("should not be called")
	}
	s.dirCreator = func(_ string) error { return nil }
	s.fileWriter = func(_ string, _ []byte) error { return nil }

	// Simulate that the markdown file exists
	startFilePath := s.getFilePath(s.URL, true)
	s.fileExistenceCheck = func(filePath string) bool {
		return filePath == startFilePath
	}

	err = s.Start(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, downloadCount, "no downloads when .md file exists")
}

// TestURLRewriteVerification empirically confirms that saved HTML has URLs
// rewritten to local paths. This justifies the decision to skip children
// for cached pages: re-parsing would yield local paths, not valid HTTP URLs.
func TestURLRewriteVerification(t *testing.T) {
	indexPage := []byte(`
<html><body>
<a href="https://example.org/page2">link</a>
<a href="/page3">another</a>
</body></html>
`)

	startURL := testStartURL
	urls := map[string][]byte{
		testStartURL: indexPage,
	}

	s := newTestScraper(t, startURL, urls)

	savedFiles := map[string][]byte{}
	s.fileWriter = func(filePath string, data []byte) error {
		savedFiles[filePath] = data
		return nil
	}

	// Only process the start page (don't follow children)
	err := s.processURL(context.Background(), s.URL, 0)
	require.NoError(t, err)

	// Find the saved HTML
	var savedHTML []byte
	for _, data := range savedFiles {
		savedHTML = data
		break
	}
	require.NotNil(t, savedHTML, "expected HTML file to be saved")

	content := string(savedHTML)
	// Verify that href values have been rewritten to local paths with .html extension
	// (not the original HTTP URLs)
	assert.Contains(t, content, "page2.html", "href should be rewritten to local .html path")
	assert.Contains(t, content, "page3.html", "href should be rewritten to local .html path")
	assert.NotContains(t, content, "https://example.org/page2", "original HTTP URL should be rewritten")
	t.Logf("Saved HTML confirms URL rewrite: %s", content)
}

func TestScraperInlineStyleAttribute(t *testing.T) {
	indexPage := []byte(`
<html>
<head>
</head>
<body>
<div style="background-image: url('https://example.org/hero.jpg')">
  <section style="background: url('/img/section-bg.png') no-repeat">
    <p style="color: red">No URL here</p>
  </section>
</div>
<span style="background-image: url(icon.gif)"></span>
</body>
</html>
`)
	empty := []byte(``)

	domain := "example.org"
	file1Reference := "hero.jpg"
	file2Reference := "img/section-bg.png"
	file3Reference := "icon.gif"
	fullURL := "https://" + domain

	urls := map[string][]byte{
		fullURL + "/":                  indexPage,
		fullURL + "/" + file1Reference: empty,
		fullURL + "/" + file2Reference: empty,
		fullURL + "/" + file3Reference: empty,
	}

	scraper := newTestScraper(t, fullURL+"/", urls)
	require.NotNil(t, scraper)

	files := map[string][]byte{}
	scraper.fileWriter = func(filePath string, data []byte) error {
		files[filePath] = data
		return nil
	}

	ctx := context.Background()
	err := scraper.Start(ctx)
	require.NoError(t, err)

	expectedProcessed := set.NewFromSlice([]string{
		"/",
		"/" + file1Reference,
		"/" + file2Reference,
		"/" + file3Reference,
	})
	require.Equal(t, expectedProcessed, scraper.processed)

	ref := domain + "/index.html"
	content := string(files[ref])
	// HTML renderer escapes single quotes to &#39; in attributes.
	assert.Contains(t, content, "url(&#39;"+file1Reference+"&#39;)")
	assert.Contains(t, content, "url(&#39;"+file2Reference+"&#39;)")
	assert.Contains(t, content, "url("+file3Reference+")")
}
