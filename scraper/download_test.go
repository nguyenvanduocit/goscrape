package scraper

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cornelk/gotokit/log"
	"github.com/cornelk/gotokit/set"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCSSProcessor(t *testing.T) {
	logger := log.NewTestLogger(t)
	cfg := Config{
		URL: "http://localhost",
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	var fixtures = map[string]string{
		"url('http://localhost/uri/between/single/quote')": "http://localhost/uri/between/single/quote",
		`url("http://localhost/uri/between/double/quote")`: "http://localhost/uri/between/double/quote",
		"url(http://localhost/uri)":                        "http://localhost/uri",
		"url(data:image/gif;base64,R0lGODl)":               "",
		`div#gopher {
			background: url(/doc/gopher/frontpage.png) no-repeat;
			height: 155px;
			}`: "http://localhost/doc/gopher/frontpage.png",
	}

	u, _ := url.Parse("http://localhost")
	for input, expected := range fixtures {
		// Drain any previously submitted tasks from the queue channel.
		drainAll(s.queue)

		// Reset the processed set so each fixture starts clean —
		// submitAsset dedup-checks before enqueue.
		s.processedMu.Lock()
		s.processed = set.New[string]()
		s.processedMu.Unlock()

		s.cssProcessor(u, []byte(input))

		if expected == "" {
			// No asset should have been submitted — brief wait then check.
			urls := collectURLs(s.queue, 0)
			assert.Empty(t, urls, "expected no assets for input: %s", input)
			continue
		}

		// Expect exactly 1 asset URL.
		urls := collectURLs(s.queue, 1)
		assert.NotEmpty(t, urls, "expected asset URL for input: %s", input)
		if len(urls) > 0 {
			assert.Equal(t, expected, urls[0])
		}
	}
}

// drainAll empties all pending tasks from the queue channel. Blocks
// briefly to let any buffering goroutines land.
func drainAll(q *taskQueue) {
	for {
		select {
		case <-q.ch:
			q.wg.Done()
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

// collectURLs reads up to `expect` task URLs from the queue, waiting up
// to 2 seconds total. If expect is 0, it does a quick non-blocking drain
// and returns whatever was there.
func collectURLs(q *taskQueue, expect int) []string {
	if expect == 0 {
		// Quick drain: brief wait then read whatever is buffered.
		time.Sleep(50 * time.Millisecond)
		var urls []string
		for {
			select {
			case t := <-q.ch:
				q.wg.Done()
				urls = append(urls, t.url.String())
			default:
				return urls
			}
		}
	}

	var urls []string
	deadline := time.After(2 * time.Second)
	for len(urls) < expect {
		select {
		case t := <-q.ch:
			q.wg.Done()
			urls = append(urls, t.url.String())
		case <-deadline:
			return urls
		}
	}
	return urls
}

// TestDownloadAssetStreaming verifies that assets with no processor are written
// to disk via the streaming path and that the buffered httpDownloader is never called.
func TestDownloadAssetStreaming(t *testing.T) {
	// Build a 6 MB body (larger than the 5 MB threshold mentioned in docs, but
	// the streaming decision is purely processor==nil, so any size triggers it).
	const assetSize = 6 * 1024 * 1024
	assetBody := bytes.Repeat([]byte{0xAB}, assetSize)

	var bufferedCalled bool
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", assetSize))
		_, _ = w.Write(assetBody)
	}))
	defer svr.Close()

	outputDir := t.TempDir()
	logger := log.NewTestLogger(t)
	cfg := Config{
		URL:             svr.URL,
		OutputDirectory: outputDir,
	}
	s, err := New(logger, cfg)
	require.NoError(t, err)

	// Redirect the buffered downloader to a sentinel so we can detect misuse.
	s.httpDownloader = func(_ context.Context, _ *url.URL) ([]byte, *url.URL, error) {
		bufferedCalled = true
		return nil, nil, fmt.Errorf("buffered path should not be called for no-processor asset")
	}

	// fileExistenceCheck must say "not cached" so downloadAsset doesn't short-circuit.
	s.fileExistenceCheck = func(_ string) bool { return false }

	assetURL, err := url.Parse(svr.URL + "/video.mp4")
	require.NoError(t, err)

	ctx := context.Background()
	// processor == nil → streaming path
	err = s.downloadAsset(ctx, assetURL, nil)
	require.NoError(t, err)

	assert.False(t, bufferedCalled, "buffered httpDownloader must not be called for no-processor asset")

	// Confirm the file was written with correct content.
	filePath := s.getFilePath(assetURL, false)
	data, err := os.ReadFile(filePath)
	require.NoError(t, err, "streamed file should exist at %s", filePath)
	assert.Equal(t, assetSize, len(data), "file size should match response body")
	assert.Equal(t, assetBody, data, "file content should match response body")
}
