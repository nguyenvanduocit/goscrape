package scraper

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cornelk/goscrape/htmlindex"
	"github.com/cornelk/gotokit/httpclient"
	"github.com/cornelk/gotokit/log"
	"github.com/cornelk/gotokit/set"
	"github.com/h2non/filetype"
	"github.com/h2non/filetype/types"
	"github.com/schollz/progressbar/v3"
	"github.com/temoto/robotstxt"
	"golang.org/x/net/html"
	"golang.org/x/time/rate"
)

// Config contains the scraper configuration.
type Config struct {
	URL      string
	Includes []string
	Excludes []string

	ImageQuality uint // image quality from 0 to 100%, 0 to disable reencoding
	MaxDepth     uint // download depth, 0 for unlimited
	Timeout      uint // time limit in seconds to process each http request

	OutputDirectory string
	Username        string
	Password        string

	Cookies   []Cookie
	Header    http.Header
	Proxy     string
	UserAgent string

	SkipExternalResources bool // skip external resources, only scrape URLs from the same domain

	// AllowCDN specifies external domains allowed for asset downloads.
	// Only used when SkipExternalResources is true.
	AllowCDN []string

	// Concurrency and rate limiting
	Concurrency int     // number of concurrent asset downloads
	RateLimit   float64 // max requests per second, 0 for unlimited
	Delay       uint    // milliseconds delay between requests

	// Progress and logging
	ShowProgress bool   // show progress bar
	ErrorLogFile string // file to log failed URLs

	// robots.txt
	RespectRobots bool // respect robots.txt rules

	// Skip403 silently skips asset URLs that return 403 Forbidden instead of logging errors.
	// Note: webpage 403s are always skipped with a warning regardless of this flag;
	// this flag only controls whether asset 403 errors are logged or silenced.
	Skip403 bool

	// DisableDefaultExcludes disables the built-in deny list for auth/special pages
	// (login, signup, MediaWiki Special:*, wp-admin, etc.).
	DisableDefaultExcludes bool

	// Markdown converts HTML pages to Markdown (.md) instead of saving HTML.
	Markdown bool

	// SkipExisting skips downloading a webpage if its target file already exists on disk.
	// Cached files cannot be re-parsed for child links because URL references have
	// already been rewritten to local paths. Children of skipped pages will not be
	// queued. A one-time warning is logged at Start().
	SkipExisting bool

	// IncludeQueryInPath, when true, makes the dedup key and on-disk filename
	// distinguish URLs that differ only in their query string. Default is false
	// (queries are dropped) because including them can explode crawl size when
	// the target site uses tracking/session params. Enable when scraping sites
	// where ?id=1 vs ?id=2 represent genuinely different content.
	IncludeQueryInPath bool

	// OnEvent is an optional consumer callback for scraping lifecycle events.
	// When set, the scraper emits events for page/asset downloads, skips, and
	// failures. Handlers MUST be non-blocking — they run on the scraper goroutine.
	OnEvent EventHandler
}

type (
	httpDownloader     func(ctx context.Context, u *url.URL) ([]byte, *url.URL, error)
	httpStreamer       func(ctx context.Context, u *url.URL, filePath string) (*url.URL, error)
	dirCreator         func(path string) error
	fileExistenceCheck func(filePath string) bool
	fileWriter         func(filePath string, data []byte) error
)

// ErrBlockedPrivateAddress is wrapped into the dial error when SSRF protection
// rejects a connection. Exported as a sentinel so retry logic can recognize
// the permanent failure via errors.Is and avoid wasting retries on it.
var ErrBlockedPrivateAddress = errors.New("blocked request to private/internal address")

// blockedIPRanges contains IP ranges that should be blocked for SSRF protection
// beyond what Go's IsLoopback/IsPrivate/IsLinkLocal covers.
var blockedIPRanges []*net.IPNet

func init() {
	for _, cidr := range []string{
		"100.64.0.0/10",      // CGNAT / RFC 6598
		"0.0.0.0/8",          // This network / RFC 1122
		"192.0.2.0/24",       // TEST-NET-1 / RFC 5737
		"198.51.100.0/24",    // TEST-NET-2 / RFC 5737
		"203.0.113.0/24",     // TEST-NET-3 / RFC 5737
		"240.0.0.0/4",        // Reserved / RFC 1112
		"255.255.255.255/32", // Broadcast
	} {
		_, n, _ := net.ParseCIDR(cidr)
		blockedIPRanges = append(blockedIPRanges, n)
	}
}

func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, cidr := range blockedIPRanges {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// FailedURL represents a URL that failed to download.
type FailedURL struct {
	URL       string
	Error     string
	Timestamp time.Time
}

// Scraper contains all scraping data.
type Scraper struct {
	config  Config
	cookies *cookiejar.Jar
	logger  *log.Logger
	URL     *url.URL // contains the main URL to parse, will be modified in case of a redirect

	auth   string
	client *http.Client

	includes []*regexp.Regexp
	excludes []*regexp.Regexp

	// key is the URL of page or asset
	processed   set.Set[string]
	processedMu sync.Mutex // protects processed set during concurrent access

	// queue is the unified work queue shared by all workers. Initialized
	// in New() so processors called from tests don't panic on nil.
	queue *taskQueue

	dirCreator         dirCreator
	fileExistenceCheck fileExistenceCheck
	fileWriter         fileWriter
	httpDownloader     httpDownloader
	httpStreamer       httpStreamer

	// Rate limiting (per-host)
	hostLimiter *hostLimiter

	// Progress tracking
	progress *progressbar.ProgressBar
	doneURLs int64 // atomic counter for completed downloads

	// Error tracking
	failedURLs   []FailedURL
	failedURLsMu sync.Mutex

	// robots.txt
	robotsData *robotstxt.RobotsData

	// allowedCDN contains external domains allowed for asset downloads
	allowedCDN set.Set[string]

	// casePathRegistry maps a lowercased filesystem path to the casePathClaim
	// (cased path + URL identity) that first claimed it. Used to disambiguate
	// URLs that differ only in case (e.g. /Logo.png vs /logo.png) on
	// case-insensitive filesystems (macOS, Windows). See disambiguateCaseCollision.
	casePathRegistry sync.Map
}

// casePathClaim records both the cased filesystem path that won a lowercased
// registry slot AND the URL that claimed it. The synthetic flag distinguishes
// natural claims (URL's own filepath mapping) from collision-suffix claims
// (allocated by the disambiguation loop). Together these let the logic
// distinguish:
//   - same URL re-asking (return previously-claimed path)
//   - different URLs that legitimately share a filepath, e.g. via query-drop
//     (return as-is, accept intentional same-target write)
//   - different URLs whose paths only differ in case (allocate a new suffixed
//     filename so they don't clobber on macOS/Windows)
//   - a literal URL that happens to match a previously-allocated synthetic
//     slot (force a fresh suffix so neither URL is silently clobbered)
type casePathClaim struct {
	cased     string
	url       string
	synthetic bool
}

// New creates a new Scraper instance.
// nolint: funlen,cyclop
func New(logger *log.Logger, cfg Config) (*Scraper, error) {
	var errs []error

	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}
	u.Fragment = ""

	includes, err := compileRegexps(cfg.Includes)
	if err != nil {
		errs = append(errs, err)
	}

	excludes, err := compileRegexps(cfg.Excludes)
	if err != nil {
		errs = append(errs, err)
	}

	if errs != nil {
		return nil, errors.Join(errs...)
	}

	if u.Scheme == "" {
		u.Scheme = "http" // if no URL scheme was given default to http
	}

	cookies, err := createCookieJar(u, cfg.Cookies)
	if err != nil {
		return nil, err
	}

	// Create HTTP transport with proxy configuration
	transport, err := httpclient.ProxyTransportFromConfig(cfg.Proxy)
	if err != nil {
		return nil, fmt.Errorf("creating proxy transport: %w", err)
	}

	// Normalize concurrency once so both transport tuning and the worker
	// pool read the same value.
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	concurrency := cfg.Concurrency

	// Tune transport for connection reuse under concurrent load.
	// Override DisableKeepAlives (set to true by gotokit SOCKS5 transport) to allow reuse.
	transport.DisableKeepAlives = false
	transport.MaxIdleConns = max(100, concurrency*4)
	transport.MaxIdleConnsPerHost = max(concurrency*2, 16)
	transport.MaxConnsPerHost = concurrency * 2
	transport.IdleConnTimeout = 60 * time.Second
	transport.ForceAttemptHTTP2 = true
	transport.WriteBufferSize = 32 << 10
	transport.ReadBufferSize = 32 << 10

	// Capture the existing DialContext (set by SOCKS5 proxy) before wrapping it.
	// If nil, default to a plain net.Dialer so SOCKS5 routing is preserved.
	prevDial := transport.DialContext
	if prevDial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second}
		prevDial = d.DialContext
	}

	// Wrap dialer to block requests to private/loopback addresses (SSRF protection).
	// The main scrape target host is always user-provided and trusted.
	targetHost := u.Hostname()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("splitting host:port: %w", err)
		}
		// Allow connections to the main scrape target (or skip check if no target configured).
		// Use prevDial so SOCKS5 routes target traffic correctly.
		if targetHost == "" || host == targetHost {
			return prevDial(ctx, network, net.JoinHostPort(host, port))
		}
		ips, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolving host: %w", err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses resolved for host: %s", host)
		}
		for _, ipStr := range ips {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				continue
			}
			if isBlockedIP(ip) {
				return nil, fmt.Errorf("%w: %s (%s)", ErrBlockedPrivateAddress, host, ipStr)
			}
		}
		// Dial the verified IP directly to prevent DNS rebinding attacks.
		// Use prevDial so SOCKS5 routing is preserved for non-target hosts.
		return prevDial(ctx, network, net.JoinHostPort(ips[0], port))
	}

	client := &http.Client{
		Jar:       cookies,
		Timeout:   time.Duration(cfg.Timeout) * time.Second,
		Transport: transport,
	}

	s := &Scraper{
		config:  cfg,
		cookies: cookies,
		logger:  logger,
		URL:     u,

		client: client,

		includes: includes,
		excludes: excludes,

		processed: set.New[string](),

		// Initialize the unified task queue with a generous buffer so
		// most submits complete without spawning a goroutine. The queue
		// is created here (not in Start) so processors called from unit
		// tests don't panic on nil.
		queue: newTaskQueue(concurrency * 64),

		allowedCDN: set.NewFromSlice(cfg.AllowCDN),
	}

	s.dirCreator = s.createDownloadPath
	s.fileExistenceCheck = s.fileExists
	s.fileWriter = s.writeFile
	s.httpDownloader = s.downloadURLWithRetries
	s.httpStreamer = s.downloadURLStreaming

	if s.config.Username != "" {
		s.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(s.config.Username+":"+s.config.Password))
	}

	// Initialize per-host rate limiter if configured
	if cfg.RateLimit > 0 {
		s.hostLimiter = newHostLimiter(rate.Limit(cfg.RateLimit), cfg.Concurrency)
	}

	return s, nil
}

// Start starts the scraping.
func (s *Scraper) Start(ctx context.Context) error {
	if err := s.dirCreator(s.config.OutputDirectory); err != nil {
		return err
	}
	s.initProgressBar()
	if s.config.SkipExisting {
		s.logger.Warn("--skip-existing: cached pages cannot be re-parsed for child links (URLs are rewritten to local paths). Children of skipped pages will NOT be discovered. Re-run without --skip-existing for a full crawl.")
	}
	if s.config.RespectRobots {
		s.fetchRobotsTxt(ctx)
	}

	if !s.shouldURLBeDownloaded(s.URL, 0, false) {
		return errors.New("start page is excluded from downloading")
	}

	// Announce the start URL as the root of the discovery tree so TUI
	// consumers can render it before its download begins.
	s.emit(Event{Kind: EventPageDiscovered, URL: s.URL.String(), Parent: "", Depth: 0})

	// Process the start page synchronously so that redirect handling
	// (s.URL rewrite) completes before any workers read s.URL for host
	// comparisons in shouldURLBeDownloaded.
	if err := s.processURL(ctx, s.URL, 0); err != nil {
		return err
	}

	s.runWorkers(ctx)

	if s.progress != nil {
		_ = s.progress.Finish()
	}

	// Surface cancellation so callers can distinguish a clean finish from
	// a mid-flight abort (the TUI uses this to skip the success path).
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("scrape aborted: %w", err)
	}
	return nil
}

// initProgressBar installs a CLI progress bar when configured. Has no effect
// when --no-progress is set or when running under the TUI.
func (s *Scraper) initProgressBar() {
	if !s.config.ShowProgress {
		return
	}
	s.progress = progressbar.NewOptions(-1,
		progressbar.OptionSetDescription("Downloading"),
		progressbar.OptionShowCount(),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionClearOnFinish(),
	)
}

// runWorkers spawns the worker pool, kicks off the queue closer, and blocks
// until every worker has exited (the queue closer closes the channel once
// all in-flight tasks complete, which lets workers fall out of their range
// loops).
func (s *Scraper) runWorkers(ctx context.Context) {
	var workerWg sync.WaitGroup
	for range s.config.Concurrency {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for t := range s.queue.ch {
				// On cancellation, skip work but ALWAYS drain and
				// call wg.Done — otherwise closeWhenDone hangs.
				if ctx.Err() == nil {
					s.processTask(ctx, t)
				}
				if t.kind == taskPage {
					s.queue.pendingPages.Add(-1)
					s.emitQueueChanged()
				}
				s.queue.wg.Done()
			}
		}()
	}
	go s.queue.closeWhenDone()
	workerWg.Wait()
}

// processTask dispatches a task to the appropriate handler based on its kind.
func (s *Scraper) processTask(ctx context.Context, t task) {
	switch t.kind {
	case taskPage:
		if err := s.processURL(ctx, t.url, t.depth); err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				s.logger.Error("Processing page failed",
					log.String("url", t.url.String()),
					log.Err(err))
			}
		}
	case taskAsset:
		// downloadAsset logs its own errors; discard the return.
		_ = s.downloadAsset(ctx, t.url, t.processor)
	}
}

func (s *Scraper) processURL(ctx context.Context, u *url.URL, currentDepth uint) error {
	s.logger.Info("Downloading webpage", log.String("url", u.String()))
	s.emit(Event{Kind: EventPageStart, URL: u.String(), Depth: currentDepth})

	if s.skipIfPageCached(u, currentDepth) {
		return nil
	}

	data, respURL, err := s.httpDownloader(ctx, u)
	if err != nil {
		return s.handlePageDownloadError(u, currentDepth, err)
	}

	fileExtension := detectFileExtension(data)
	if currentDepth == 0 {
		u = s.adoptStartPageRedirect(u, respURL)
	}

	doc, err := html.Parse(bytes.NewBuffer(data))
	if err != nil {
		s.logger.Error("Parsing HTML failed",
			log.String("url", u.String()),
			log.Err(err))
		return fmt.Errorf("parsing HTML: %w", err)
	}

	index := htmlindex.New(s.logger)
	index.Index(u, doc)

	if storeErr := s.storeDownload(u, data, doc, index, fileExtension); storeErr != nil {
		s.addFailedURL(u.String(), storeErr)
		s.emit(Event{Kind: EventFailed, URL: u.String(), Err: storeErr.Error(), Depth: currentDepth})
		return storeErr
	}
	s.submitReferences(index)

	if s.queueChildPages(index, u, currentDepth) {
		s.emitQueueChanged()
	}
	s.emit(Event{Kind: EventPageDone, URL: u.String(), Depth: currentDepth})
	return nil
}

// skipIfPageCached returns true when --skip-existing is set and the page's
// target file already exists on disk. Emits the skipped event so consumers
// see the lifecycle close.
//
// Cached files have their URL references rewritten to local paths, so
// re-parsing them would yield invalid URLs — children are skipped entirely.
func (s *Scraper) skipIfPageCached(u *url.URL, currentDepth uint) bool {
	if !s.config.SkipExisting {
		return false
	}
	filePath := s.getFilePath(u, true)
	if !s.fileExistenceCheck(filePath) {
		return false
	}
	s.logger.Debug("Skipping existing page",
		log.String("url", u.String()),
		log.String("file", filePath))
	s.incrementProgress()
	s.emit(Event{Kind: EventSkipped, URL: u.String(), Message: "existing on disk", Depth: currentDepth})
	return true
}

// handlePageDownloadError translates a download error into a per-depth
// outcome: at depth 0 a 403 aborts the scrape; deeper, 403 is recorded
// and crawling continues. Any other error is logged and propagated.
func (s *Scraper) handlePageDownloadError(u *url.URL, currentDepth uint, err error) error {
	if IsHTTPStatusError(err, http.StatusForbidden) {
		s.addFailedURL(u.String(), err)
		s.incrementProgress()
		if currentDepth == 0 {
			return fmt.Errorf("start page returned 403 Forbidden: %s", u.String())
		}
		s.logger.Warn("HTTP 403 on webpage, skipping", log.String("url", u.String()))
		s.emit(Event{Kind: EventFailed, URL: u.String(), Err: "403 Forbidden", Depth: currentDepth})
		return nil
	}
	s.logger.Error("Processing HTTP Request failed",
		log.String("url", u.String()),
		log.Err(err))
	s.emit(Event{Kind: EventFailed, URL: u.String(), Err: err.Error(), Depth: currentDepth})
	return err
}

// adoptStartPageRedirect rewrites the scraper's base URL when the start
// page returned a redirect. Announces the post-redirect URL as discovered
// so TUI consumers thread children under the right parent.
func (s *Scraper) adoptStartPageRedirect(original, respURL *url.URL) *url.URL {
	if respURL == nil {
		return original
	}
	if respURL.String() != original.String() {
		s.emit(Event{
			Kind:    EventPageDiscovered,
			URL:     respURL.String(),
			Parent:  original.String(),
			Depth:   0,
			Message: "redirected from start URL",
		})
	}
	s.URL = respURL
	return respURL
}

// queueChildPages submits every <a> href that should be crawled and emits
// a discovery edge per submission. Returns true if at least one task was
// queued (caller emits the queue-change event in that case).
//
// Check-and-submit happens first; download is async — see scraper.Start.
// The MaxDepth check fires inside shouldURLBeDownloaded, so we don't need
// to gate the loop here.
func (s *Scraper) queueChildPages(index *htmlindex.Index, parent *url.URL, currentDepth uint) bool {
	references, err := index.URLs(htmlindex.ATag)
	if err != nil {
		s.logger.Error("Parsing URL failed", log.Err(err))
	}
	queued := false
	for _, ur := range references {
		ur.Fragment = ""
		if !s.shouldURLBeDownloaded(ur, currentDepth, false) {
			continue
		}
		s.queue.submit(task{
			kind:   taskPage,
			url:    ur,
			depth:  currentDepth + 1,
			parent: parent.String(),
		})
		queued = true
		s.emit(Event{
			Kind:   EventPageDiscovered,
			URL:    ur.String(),
			Parent: parent.String(),
			Depth:  currentDepth + 1,
		})
	}
	return queued
}

// detectFileExtension classifies the downloaded bytes via magic-number
// matching. Returns "" if the type is unknown (HTML/text/etc.).
func detectFileExtension(data []byte) string {
	kind, err := filetype.Match(data)
	if err != nil || kind == types.Unknown {
		return ""
	}
	return kind.Extension
}

// storeDownload writes the download to a file, if a known binary file is detected,
// processing of the file as page to look for links is skipped.
func (s *Scraper) storeDownload(u *url.URL, data []byte, doc *html.Node,
	index *htmlindex.Index, fileExtension string) error {

	// We need to distinguish between HTML pages and binary files (images, PDFs, etc.)
	// because they need different file path handling:
	// - HTML pages: add .html/.md extension, handle directory indexes
	// - Binary files: keep original path, so /photo.jpg stays /photo.jpg
	isHTML := false
	if fileExtension == "" {
		fixed, hasChanges, err := s.fixURLReferences(u, doc, index)
		if err != nil {
			s.logger.Error("Fixing file references failed",
				log.String("url", u.String()),
				log.Err(err))
			// Write raw data as fallback instead of skipping the page entirely
		} else if hasChanges {
			data = fixed
		}
		// Only HTML content gets processed as a "page" - binary files stay as-is
		isHTML = true
	}

	// In Markdown mode, convert the (URL-rewritten) HTML doc to Markdown.
	if isHTML && s.config.Markdown {
		md, err := s.convertToMarkdown(doc, u)
		if err != nil {
			s.logger.Error("Markdown conversion failed",
				log.String("url", u.String()),
				log.Err(err))
			// Fallback: keep HTML data
		} else {
			data = md
		}
	}

	filePath := s.getFilePath(u, isHTML)
	if err := s.fileWriter(filePath, data); err != nil {
		s.logger.Error("Writing to file failed",
			log.String("URL", u.String()),
			log.String("file", filePath),
			log.Err(err))
		return fmt.Errorf("writing page file %q: %w", filePath, err)
	}
	return nil
}

// compileRegexps compiles the given regex strings to regular expressions
// to be used in the include and exclude filters.
func compileRegexps(regexps []string) ([]*regexp.Regexp, error) {
	var errs []error
	var compiled []*regexp.Regexp

	for _, exp := range regexps {
		re, err := regexp.Compile(exp)
		if err == nil {
			compiled = append(compiled, re)
		} else {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return compiled, nil
}

// FailedURLs returns the list of URLs that failed to download.
func (s *Scraper) FailedURLs() []FailedURL {
	s.failedURLsMu.Lock()
	defer s.failedURLsMu.Unlock()
	return append([]FailedURL{}, s.failedURLs...)
}

// addFailedURL records a failed URL download.
func (s *Scraper) addFailedURL(u string, err error) {
	s.failedURLsMu.Lock()
	defer s.failedURLsMu.Unlock()
	s.failedURLs = append(s.failedURLs, FailedURL{
		URL:       u,
		Error:     err.Error(),
		Timestamp: time.Now(),
	})
}

// incrementProgress increments the progress counter and updates the progress bar.
func (s *Scraper) incrementProgress() {
	atomic.AddInt64(&s.doneURLs, 1)
	if s.progress != nil {
		_ = s.progress.Add(1)
	}
}

// fetchRobotsTxt fetches and parses robots.txt for the main domain.
// Uses a plain HTTP client without auth credentials to avoid leaking
// them to the robots.txt endpoint or via redirects.
func (s *Scraper) fetchRobotsTxt(ctx context.Context) {
	robotsURL := &url.URL{
		Scheme: s.URL.Scheme,
		Host:   s.URL.Host,
		Path:   "/robots.txt",
	}

	s.logger.Debug("Fetching robots.txt", log.String("url", robotsURL.String()))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL.String(), nil)
	if err != nil {
		s.logger.Debug("Failed to create robots.txt request", log.Err(err))
		return
	}
	if s.config.UserAgent != "" {
		req.Header.Set("User-Agent", s.config.UserAgent)
	}

	// Use a client without auth headers to avoid credential leakage
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		s.logger.Debug("Failed to fetch robots.txt, ignoring", log.Err(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		s.logger.Debug("robots.txt returned non-200 status, ignoring")
		return
	}

	const maxRobotsTxtSize int64 = 512 * 1024
	buf := &bytes.Buffer{}
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, maxRobotsTxtSize)); err != nil {
		s.logger.Debug("Failed to read robots.txt body, ignoring", log.Err(err))
		return
	}

	robots, err := robotstxt.FromBytes(buf.Bytes())
	if err != nil {
		s.logger.Debug("Failed to parse robots.txt, ignoring", log.Err(err))
		return
	}

	s.robotsData = robots
	s.logger.Debug("Loaded robots.txt successfully")
}
