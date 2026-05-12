package scraper

import "time"

// EventKind identifies the kind of scraping event emitted to consumers
// (e.g. the TUI). Kinds are coarse-grained so the TUI can render them
// without coupling to scraper internals.
type EventKind int

const (
	// EventPageStart is emitted right before a webpage download begins.
	EventPageStart EventKind = iota
	// EventPageDone is emitted after a webpage is downloaded and stored.
	EventPageDone
	// EventAssetStart is emitted right before an asset download begins.
	EventAssetStart
	// EventAssetDone is emitted after an asset is downloaded and stored.
	EventAssetDone
	// EventSkipped is emitted when a URL is skipped (existing on disk, excluded, 403 with skip-403, etc.).
	EventSkipped
	// EventFailed is emitted when a URL fails to download.
	EventFailed
	// EventQueueChanged is emitted when the pending webpage queue size changes.
	EventQueueChanged
	// EventLog is a generic log line surfaced to the consumer.
	EventLog
	// EventPageDiscovered is emitted when a new page URL is queued for download.
	// The Parent field carries the URL of the page that linked to it
	// (empty for the start page). Consumers use this to build a discovery
	// tree before the page is actually downloaded.
	EventPageDiscovered
)

// Event is a snapshot delivered to the consumer for a single scraping step.
// All fields are filled best-effort; consumers must tolerate empty values.
type Event struct {
	Kind EventKind
	URL  string
	// Parent is the URL that linked to this page. Set only for
	// EventPageDiscovered. Empty for the start page (root).
	Parent    string
	Message   string
	Err       string
	QueueSize int
	Depth     uint
	Timestamp time.Time
}

// EventHandler is the consumer callback invoked synchronously from the scraper.
// Handlers MUST NOT block — push to a channel and return.
type EventHandler func(Event)

// emit is a tiny helper that fills in the timestamp and dispatches the event
// if a handler is configured. It is safe to call when no handler is set.
func (s *Scraper) emit(ev Event) {
	if s.config.OnEvent == nil {
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	s.config.OnEvent(ev)
}

// emitQueueChanged emits a queue-size update using the pending page counter.
func (s *Scraper) emitQueueChanged() {
	if s.config.OnEvent == nil {
		return
	}
	size := int(s.queue.pendingPages.Load())
	if size < 0 {
		size = 0
	}
	s.emit(Event{Kind: EventQueueChanged, QueueSize: size})
}
