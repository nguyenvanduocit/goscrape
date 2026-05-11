package scraper

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/cornelk/goscrape/htmlindex"
	"github.com/cornelk/gotokit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

func testURL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse("https://example.org/")
	require.NoError(t, err)
	return u
}

func TestConvertToMarkdown(t *testing.T) {
	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:      "https://example.org/",
		Markdown: true,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	input := `<html><head><title>Test</title></head><body><h1>Hello</h1><p>World</p></body></html>`
	doc, err := html.Parse(strings.NewReader(input))
	require.NoError(t, err)

	md, err := s.convertToMarkdown(doc, testURL(t))
	require.NoError(t, err)

	content := string(md)
	assert.Contains(t, content, "# Hello")
	assert.Contains(t, content, "World")
	// Should not contain raw HTML tags
	assert.NotContains(t, content, "<h1>")
	assert.NotContains(t, content, "<p>")
}

func TestConvertToMarkdownLinks(t *testing.T) {
	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:      "https://example.org/",
		Markdown: true,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	input := `<html><body><a href="about.md">About</a></body></html>`
	doc, err := html.Parse(strings.NewReader(input))
	require.NoError(t, err)

	md, err := s.convertToMarkdown(doc, testURL(t))
	require.NoError(t, err)

	content := string(md)
	assert.Contains(t, content, "[About](about.md)")
}

func TestConvertToMarkdownImages(t *testing.T) {
	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:      "https://example.org/",
		Markdown: true,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	input := `<html><body><img src="photo.png" alt="A photo"/></body></html>`
	doc, err := html.Parse(strings.NewReader(input))
	require.NoError(t, err)

	md, err := s.convertToMarkdown(doc, testURL(t))
	require.NoError(t, err)

	content := string(md)
	assert.Contains(t, content, "![A photo](photo.png)")
}

func TestPageExtension(t *testing.T) {
	logger := log.NewTestLogger(t)

	// Default mode: .html
	cfg := Config{URL: "https://example.org/"}
	s, err := New(logger, cfg)
	require.NoError(t, err)
	assert.Equal(t, PageExtension, s.pageExtension())

	// Markdown mode: .md
	cfg.Markdown = true
	s, err = New(logger, cfg)
	require.NoError(t, err)
	assert.Equal(t, MarkdownExtension, s.pageExtension())
}

func TestGetPageFilePathWithExt(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		ext      string
		expected string
	}{
		{"root html", "/", ".html", "index.html"},
		{"root md", "/", ".md", "index.md"},
		{"empty html", "", ".html", "index.html"},
		{"empty md", "", ".md", "index.md"},
		{"dir html", "/about/", ".html", "/about/index.html"},
		{"dir md", "/about/", ".md", "/about/index.md"},
		{"no ext html", "/about", ".html", "/about.html"},
		{"no ext md", "/about", ".md", "/about.md"},
		// Markdown mode must rewrite dynamic-page extensions because the file
		// content is markdown, not the original page type.
		{"php to md", "/index.php", ".md", "/index.md"},
		{"aspx to md", "/page.aspx", ".md", "/page.md"},
		{"html to md", "/about.html", ".md", "/about.md"},
		// HTML mode preserves dynamic-page extensions (backward compat).
		{"aspx kept in html mode", "/page.aspx", ".html", "/page.aspx"},
		{"php kept in html mode", "/index.php", ".html", "/index.php"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &url.URL{Path: tt.path}
			result := getPageFilePathWithExt(u, tt.ext)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMarkdownFilePathIntegration(t *testing.T) {
	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:      "https://example.org/",
		Markdown: true,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	u, err := url.Parse("https://example.org/about")
	require.NoError(t, err)

	filePath := s.getFilePath(u, true)
	assert.True(t, strings.HasSuffix(filePath, "about.md"), "expected .md suffix, got: %s", filePath)

	u, err = url.Parse("https://example.org/")
	require.NoError(t, err)

	filePath = s.getFilePath(u, true)
	assert.True(t, strings.HasSuffix(filePath, "index.md"), "expected index.md, got: %s", filePath)
}

func TestMarkdownSkipsCSSJS(t *testing.T) {
	indexPage := []byte(`
<html>
<head>
<link href='https://example.org/style.css' rel='stylesheet' type='text/css'>
<script src='https://example.org/app.js'></script>
</head>
<body>
<img src='https://example.org/photo.jpg'/>
</body>
</html>
`)

	startURL := "https://example.org/"
	urls := map[string][]byte{
		"https://example.org/":          indexPage,
		"https://example.org/style.css": []byte(`body{}`),
		"https://example.org/app.js":    []byte(`console.log("hi")`),
		"https://example.org/photo.jpg": []byte(`fake-jpg-data`),
	}

	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:      startURL,
		Markdown: true,
	}
	scraper, err := New(logger, cfg)
	require.NoError(t, err)

	scraper.dirCreator = func(_ string) error { return nil }
	scraper.fileExistenceCheck = func(_ string) bool { return false }

	files := map[string][]byte{}
	scraper.fileWriter = func(filePath string, data []byte) error {
		files[filePath] = data
		return nil
	}
	scraper.httpDownloader = func(_ context.Context, u *url.URL) ([]byte, *url.URL, error) {
		ur := u.String()
		b, ok := urls[ur]
		if ok {
			return b, u, nil
		}
		return nil, nil, fmt.Errorf("url '%s' not found in test data", ur)
	}

	ctx := context.Background()
	err = scraper.Start(ctx)
	require.NoError(t, err)

	// Image should be downloaded
	hasImage := false
	hasCSS := false
	hasJS := false
	hasMarkdown := false
	for path := range files {
		if strings.HasSuffix(path, ".jpg") {
			hasImage = true
		}
		if strings.HasSuffix(path, ".css") {
			hasCSS = true
		}
		if strings.HasSuffix(path, ".js") {
			hasJS = true
		}
		if strings.HasSuffix(path, ".md") {
			hasMarkdown = true
		}
	}

	assert.True(t, hasImage, "images should be downloaded in markdown mode")
	assert.False(t, hasCSS, "CSS should NOT be downloaded in markdown mode")
	assert.False(t, hasJS, "JS should NOT be downloaded in markdown mode")
	assert.True(t, hasMarkdown, "markdown file should be written")
}

func TestMarkdownStoreDownload(t *testing.T) {
	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:      "https://example.org/",
		Markdown: true,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	var writtenPath string
	var writtenData []byte
	s.fileWriter = func(filePath string, data []byte) error {
		writtenPath = filePath
		writtenData = data
		return nil
	}

	htmlContent := `<html><head></head><body><h1>Test Page</h1><p>Content here.</p></body></html>`
	buf := bytes.NewBufferString(htmlContent)
	doc, err := html.Parse(buf)
	require.NoError(t, err)

	u, err := url.Parse("https://example.org/page")
	require.NoError(t, err)

	// htmlindex is needed but we can pass a nil-safe one
	idx := newTestIndex(t, u, doc)
	s.storeDownload(u, []byte(htmlContent), doc, idx, "")

	assert.True(t, strings.HasSuffix(writtenPath, "page.md"), "expected .md path, got: %s", writtenPath)
	assert.Contains(t, string(writtenData), "# Test Page")
	assert.Contains(t, string(writtenData), "Content here.")
	assert.NotContains(t, string(writtenData), "<html>")
}

func newTestIndex(t *testing.T, u *url.URL, doc *html.Node) *htmlindex.Index {
	t.Helper()
	logger := log.NewTestLogger(t)
	idx := htmlindex.New(logger)
	idx.Index(u, doc)
	return idx
}

// --- New tests for frontmatter and cleanup ---

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		name     string
		html     string
		expected string
	}{
		{"simple title", `<html><head><title>Hello World</title></head><body></body></html>`, "Hello World"},
		{"nested HTML inside title", `<html><head><title>Hello <b>Bold</b> World</title></head><body></body></html>`, "Hello <b>Bold</b> World"},
		{"missing title", `<html><head></head><body></body></html>`, ""},
		{"empty title", `<html><head><title></title></head><body></body></html>`, ""},
		{"whitespace-only title", `<html><head><title>   </title></head><body></body></html>`, ""},
		{"title with internal newlines", "<html><head><title>Hello\n\tWorld\n  Page</title></head><body></body></html>", "Hello World Page"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := html.Parse(strings.NewReader(tt.html))
			require.NoError(t, err)
			result := extractTitle(doc)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildFrontmatter(t *testing.T) {
	t.Run("with title", func(t *testing.T) {
		fm := string(buildFrontmatter("My Page", "https://example.com/", "2026-05-11T20:00:00Z"))
		assert.Equal(t, "---\ntitle: \"My Page\"\nurl: \"https://example.com/\"\nscraped_at: \"2026-05-11T20:00:00Z\"\n---\n\n", fm)
	})

	t.Run("without title", func(t *testing.T) {
		fm := string(buildFrontmatter("", "https://example.com/", "2026-05-11T20:00:00Z"))
		assert.NotContains(t, fm, "title:")
		assert.Contains(t, fm, "url: \"https://example.com/\"")
	})

	t.Run("title with special characters", func(t *testing.T) {
		fm := string(buildFrontmatter(`Say "hello" and use \backslash`, "https://example.com/", "2026-05-11T20:00:00Z"))
		assert.Contains(t, fm, `title: "Say \"hello\" and use \\backslash"`)
	})
}

func TestCleanupEmptyImageLinks(t *testing.T) {
	input := "before ![]() after"
	result := string(cleanupMarkdown([]byte(input)))
	assert.Equal(t, "before  after\n", result)
}

func TestCleanupEmptyTextOnlyLink(t *testing.T) {
	input := "see [click here]() for info"
	result := string(cleanupMarkdown([]byte(input)))
	assert.Equal(t, "see click here for info\n", result)
}

func TestCleanupEmptyHrefAutolink(t *testing.T) {
	input := "visit [](https://foo.com) now"
	result := string(cleanupMarkdown([]byte(input)))
	assert.Equal(t, "visit <https://foo.com> now\n", result)
}

func TestCleanupBothEmptyLink(t *testing.T) {
	input := "before []() after"
	result := string(cleanupMarkdown([]byte(input)))
	assert.Equal(t, "before  after\n", result)
}

func TestCleanupEmptyHeading(t *testing.T) {
	input := "#\n## \n### Real Heading"
	result := string(cleanupMarkdown([]byte(input)))
	assert.NotContains(t, result, "#\n")
	assert.NotContains(t, result, "## \n")
	assert.Contains(t, result, "### Real Heading")
}

func TestCleanupTrailingWhitespace(t *testing.T) {
	input := "text   \nother"
	result := string(cleanupMarkdown([]byte(input)))
	assert.Equal(t, "text\nother\n", result)
}

func TestCleanupCollapseBlankLines(t *testing.T) {
	input := "para1\n\n\n\n\npara2"
	result := string(cleanupMarkdown([]byte(input)))
	assert.Equal(t, "para1\n\npara2\n", result)
}

func TestCleanupPreservesFencedCodeBackticks(t *testing.T) {
	input := "before\n```\n[](https://foo)\n![]() \n## \n```\nafter"
	result := string(cleanupMarkdown([]byte(input)))
	assert.Contains(t, result, "[](https://foo)")
	assert.Contains(t, result, "![]() ")
	assert.Contains(t, result, "## ")
}

func TestCleanupPreservesFencedCodeTildes(t *testing.T) {
	input := "before\n~~~\n[]() should stay\n~~~\nafter"
	result := string(cleanupMarkdown([]byte(input)))
	assert.Contains(t, result, "[]() should stay")
}

func TestCleanupPreservesValidImageWithEmptyAlt(t *testing.T) {
	input := "![](https://example.com/img.png)"
	result := string(cleanupMarkdown([]byte(input)))
	assert.Equal(t, "![](https://example.com/img.png)\n", result)
}

func TestConvertToMarkdownIncludesFrontmatter(t *testing.T) {
	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:      "https://example.org/",
		Markdown: true,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	input := `<html><head><title>Foo</title></head><body><h1>Hi</h1></body></html>`
	doc, err := html.Parse(strings.NewReader(input))
	require.NoError(t, err)

	u, err := url.Parse("https://example.org/page")
	require.NoError(t, err)

	md, err := s.convertToMarkdown(doc, u)
	require.NoError(t, err)

	content := string(md)
	assert.True(t, strings.HasPrefix(content, "---\n"), "should start with frontmatter separator")
	assert.Contains(t, content, `title: "Foo"`)
	assert.Contains(t, content, `url: "https://example.org/page"`)
	assert.Contains(t, content, "scraped_at: \"")
	// Body should follow after closing separator
	assert.Contains(t, content, "---\n\n# Hi")
}
