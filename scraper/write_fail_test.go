package scraper

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"

	"github.com/cornelk/gotokit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventRecorder collects emitted events for assertion.
type eventRecorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *eventRecorder) handler() EventHandler {
	return func(ev Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, ev)
	}
}

func (r *eventRecorder) has(kind EventKind, url string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.Kind == kind && (url == "" || ev.URL == url) {
			return true
		}
	}
	return false
}

// TestBufferedAssetWriteFail verifies that a write failure on a buffered
// (processor-bearing) asset propagates as an error, emits EventFailed
// (not EventAssetDone), records the failure, unmarks the URL so a later
// discovery can retry, and does NOT bump doneURLs.
func TestBufferedAssetWriteFail(t *testing.T) {
	rec := &eventRecorder{}
	cfg := Config{URL: "https://example.org", OnEvent: rec.handler()}

	s, err := New(log.NewNop(), cfg)
	require.NoError(t, err)

	s.dirCreator = func(_ string) error { return nil }
	s.fileExistenceCheck = func(_ string) bool { return false }
	s.httpDownloader = func(_ context.Context, u *url.URL) ([]byte, *url.URL, error) {
		return []byte("body"), u, nil
	}
	writeErr := errors.New("disk full")
	s.fileWriter = func(_ string, _ []byte) error { return writeErr }

	u, err := url.Parse("https://example.org/asset.css")
	require.NoError(t, err)

	// Pre-mark the URL as if submitAsset/shouldURLBeDownloaded ran.
	require.True(t, s.shouldURLBeDownloaded(u, 0, true))
	require.True(t, s.processed.Contains(u.Path), "URL must be marked before downloadAsset runs")

	processor := func(_ *url.URL, data []byte) []byte { return data }
	err = s.downloadAsset(context.Background(), u, processor)
	require.ErrorIs(t, err, writeErr)
	assert.Equal(t, int64(0), s.doneURLs, "doneURLs must not advance on write fail")

	failed := s.FailedURLs()
	require.Len(t, failed, 1)
	assert.Equal(t, u.String(), failed[0].URL)
	assert.Contains(t, failed[0].Error, "writing asset file", "failed entry must carry wrapped error context")

	assert.True(t, rec.has(EventFailed, u.String()), "EventFailed must be emitted")
	assert.False(t, rec.has(EventAssetDone, u.String()), "EventAssetDone must NOT be emitted")
	assert.False(t, s.processed.Contains(u.Path), "URL must be unmarked after write fail so re-discovery can retry")
}

// TestStreamingAssetDirCreateFail verifies that a dir-create failure on the
// streaming asset path emits EventFailed, records the failure with wrapped
// context, and unmarks the URL for retry.
func TestStreamingAssetDirCreateFail(t *testing.T) {
	rec := &eventRecorder{}
	cfg := Config{URL: "https://example.org", OnEvent: rec.handler()}

	s, err := New(log.NewNop(), cfg)
	require.NoError(t, err)

	dirErr := errors.New("read-only filesystem")
	s.dirCreator = func(_ string) error { return dirErr }
	s.fileExistenceCheck = func(_ string) bool { return false }

	u, err := url.Parse("https://example.org/video.mp4")
	require.NoError(t, err)
	require.True(t, s.shouldURLBeDownloaded(u, 0, true))

	err = s.downloadAsset(context.Background(), u, nil)
	require.ErrorIs(t, err, dirErr)
	failed := s.FailedURLs()
	require.Len(t, failed, 1)
	assert.Contains(t, failed[0].Error, "creating asset directory", "failed entry must carry wrapped context")
	assert.False(t, s.processed.Contains(u.Path), "URL must be unmarked on dir-create fail")
}

// TestPageWriteFailViaStart verifies that a start-page write failure causes
// Start to return an error, emits EventFailed (not EventPageDone), and
// records the failure.
func TestPageWriteFailViaStart(t *testing.T) {
	indexPage := []byte(`<html><body><a href="/child">c</a><img src="/img.png"></body></html>`)

	rec := &eventRecorder{}
	cfg := Config{URL: "https://example.org", OnEvent: rec.handler()}

	s, err := New(log.NewNop(), cfg)
	require.NoError(t, err)

	s.dirCreator = func(_ string) error { return nil }
	s.fileExistenceCheck = func(_ string) bool { return false }
	s.httpDownloader = func(_ context.Context, u *url.URL) ([]byte, *url.URL, error) {
		return indexPage, u, nil
	}
	writeErr := errors.New("permission denied")
	s.fileWriter = func(_ string, _ []byte) error { return writeErr }

	err = s.Start(context.Background())
	require.ErrorIs(t, err, writeErr, "Start must propagate page write fail")

	require.Len(t, s.FailedURLs(), 1, "page write fail must be recorded")
	assert.True(t, rec.has(EventFailed, ""), "EventFailed must be emitted on page write fail")
	assert.False(t, rec.has(EventPageDone, ""), "EventPageDone must NOT be emitted on write fail")
}
