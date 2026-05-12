package scraper

import (
	"net/url"
	"sync"
	"sync/atomic"
)

// taskKind distinguishes page crawl tasks from asset download tasks in
// the unified worker pool.
type taskKind int

const (
	taskPage  taskKind = iota // HTML page to fetch, parse, and extract links from
	taskAsset                 // static asset (image, CSS, JS, font, etc.)
)

// task is a single unit of work consumed by a pool worker.
type task struct {
	kind      taskKind
	url       *url.URL
	depth     uint           // page tasks: depth of this page in the BFS tree
	parent    string         // page tasks: URL of the page that linked here
	processor assetProcessor // asset tasks: optional post-download transform
}

// taskQueue is a persistent, bounded channel of tasks with a WaitGroup that
// tracks outstanding work (submitted but not yet completed). The closer
// goroutine waits for the WaitGroup to drain, then closes the channel so
// workers exit their range loop.
//
// Submit uses a goroutine to buffer the channel send, preventing deadlock
// when all workers are simultaneously submitting child tasks and the
// channel buffer is full. The goroutine cost is ~2KB and short-lived.
type taskQueue struct {
	ch chan task
	wg sync.WaitGroup // tracks outstanding tasks, NOT workers

	// pendingPages is an approximate count of page tasks in the queue,
	// used for EventQueueChanged emission. Incremented on page submit,
	// decremented when a page task completes.
	pendingPages atomic.Int64
}

func newTaskQueue(buf int) *taskQueue {
	return &taskQueue{ch: make(chan task, buf)}
}

// submit enqueues a task. wg.Add is called BEFORE the channel send so the
// counter never undershoots while a producer is still computing.
func (q *taskQueue) submit(t task) {
	q.wg.Add(1)
	if t.kind == taskPage {
		q.pendingPages.Add(1)
	}
	// Buffer the send in a goroutine to prevent deadlock: if all workers
	// are blocked on their own submits and the channel is full, no worker
	// can complete to drain the channel. The goroutine breaks the cycle.
	go func() { q.ch <- t }()
}

// closeWhenDone blocks until all outstanding tasks complete, then closes
// the channel so workers exit their range loops. Must be called exactly once.
func (q *taskQueue) closeWhenDone() {
	q.wg.Wait()
	close(q.ch)
}
