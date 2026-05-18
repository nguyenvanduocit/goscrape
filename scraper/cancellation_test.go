package scraper

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/cornelk/gotokit/log"
	"github.com/stretchr/testify/require"
)

// TestStartReturnsCanceledWhenStartPageAborted — context canceled while the
// (synchronous) start-page download is in flight must surface as a wrapped
// context.Canceled error from Start().
func TestStartReturnsCanceledWhenStartPageAborted(t *testing.T) {
	s, err := New(log.NewNop(), Config{URL: "https://example.org"})
	require.NoError(t, err)

	s.dirCreator = func(_ string) error { return nil }
	s.fileExistenceCheck = func(_ string) bool { return false }
	s.fileWriter = func(_ string, _ []byte) error { return nil }

	called := make(chan struct{})
	s.httpDownloader = func(ctx context.Context, _ *url.URL) ([]byte, *url.URL, error) {
		close(called)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() { doneCh <- s.Start(ctx) }()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("downloader was never called")
	}
	cancel()

	select {
	case err := <-doneCh:
		require.ErrorIs(t, err, context.Canceled, "Start must return context.Canceled from start-page abort")
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

// TestStartReturnsCanceledDuringWorkerPhase — once the start page completes
// and workers begin draining children/assets, canceling the context must
// still surface as context.Canceled (not nil) from Start().
func TestStartReturnsCanceledDuringWorkerPhase(t *testing.T) {
	const indexHTML = `<html><body>
<a href="/c1">a</a><a href="/c2">b</a><a href="/c3">c</a>
</body></html>`

	s, err := New(log.NewNop(), Config{URL: "https://example.org", Concurrency: 2})
	require.NoError(t, err)

	s.dirCreator = func(_ string) error { return nil }
	s.fileExistenceCheck = func(_ string) bool { return false }
	s.fileWriter = func(_ string, _ []byte) error { return nil }

	startPageDone := make(chan struct{})
	childCalled := make(chan struct{}, 1)
	s.httpDownloader = func(ctx context.Context, u *url.URL) ([]byte, *url.URL, error) {
		if u.Path == "" || u.Path == "/" {
			// start page returns immediately so workers start picking up children
			close(startPageDone)
			return []byte(indexHTML), u, nil
		}
		// child pages: signal once, then block until canceled
		select {
		case childCalled <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() { doneCh <- s.Start(ctx) }()

	select {
	case <-startPageDone:
	case <-time.After(2 * time.Second):
		t.Fatal("start page never downloaded")
	}
	select {
	case <-childCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("workers never picked up a child task")
	}
	cancel()

	select {
	case err := <-doneCh:
		require.ErrorIs(t, err, context.Canceled, "Start must return context.Canceled from worker phase")
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

// TestStartReturnsNilOnCleanFinish — sanity check that the new wrap doesn't
// regress: a clean finish (no cancel) returns nil.
func TestStartReturnsNilOnCleanFinish(t *testing.T) {
	const indexHTML = `<html><body><p>ok</p></body></html>`

	s, err := New(log.NewNop(), Config{URL: "https://example.org", Concurrency: 1})
	require.NoError(t, err)

	s.dirCreator = func(_ string) error { return nil }
	s.fileExistenceCheck = func(_ string) bool { return false }
	s.fileWriter = func(_ string, _ []byte) error { return nil }
	s.httpDownloader = func(_ context.Context, u *url.URL) ([]byte, *url.URL, error) {
		return []byte(indexHTML), u, nil
	}

	require.NoError(t, s.Start(context.Background()))
}
