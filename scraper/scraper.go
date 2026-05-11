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

	// OnEvent is an optional consumer callback for scraping lifecycle events.
	// When set, the scraper emits events for page/asset downloads, skips, and
	// failures. Handlers MUST be non-blocking — they run on the scraper goroutine.
	OnEvent EventHandler
}

type (
	httpDownloader     func(ctx context.Context, u *url.URL) ([]byte, *url.URL, error)
	dirCreator         func(path string) error
	fileExistenceCheck func(filePath string) bool
	fileWriter         func(filePath string, data []byte) error
)

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

	assetQueue       []*url.URL
	assetsMu          sync.Mutex // protects assetQueue during concurrent access
	webPageQueue      []*url.URL
	webPageQueueDepth map[string]uint
	webPageMu         sync.Mutex // protects webPageQueue and webPageQueueDepth

	dirCreator         dirCreator
	fileExistenceCheck fileExistenceCheck
	fileWriter         fileWriter
	httpDownloader     httpDownloader

	// Rate limiting
	rateLimiter *rate.Limiter

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

	// Wrap dialer to block requests to private/loopback addresses (SSRF protection).
	// The main scrape target host is always user-provided and trusted.
	targetHost := u.Hostname()
	originalDialer := &net.Dialer{Timeout: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("splitting host:port: %w", err)
		}
		// Allow connections to the main scrape target (or skip check if no target configured)
		if targetHost == "" || host == targetHost {
			return originalDialer.DialContext(ctx, network, net.JoinHostPort(host, port))
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
				return nil, fmt.Errorf("blocked request to private/internal address: %s (%s)", host, ipStr)
			}
		}
		// Dial the verified IP directly to prevent DNS rebinding attacks
		return originalDialer.DialContext(ctx, network, net.JoinHostPort(ips[0], port))
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

		webPageQueueDepth: map[string]uint{},
		allowedCDN:        set.NewFromSlice(cfg.AllowCDN),
	}

	s.dirCreator = s.createDownloadPath
	s.fileExistenceCheck = s.fileExists
	s.fileWriter = s.writeFile
	s.httpDownloader = s.downloadURLWithRetries

	if s.config.Username != "" {
		s.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(s.config.Username+":"+s.config.Password))
	}

	// Initialize rate limiter if configured
	if cfg.RateLimit > 0 {
		burst := cfg.Concurrency
		if burst < 1 {
			burst = 1
		}
		s.rateLimiter = rate.NewLimiter(rate.Limit(cfg.RateLimit), burst)
	}

	// Set default concurrency
	if s.config.Concurrency <= 0 {
		s.config.Concurrency = 1
	}

	return s, nil
}

// Start starts the scraping.
func (s *Scraper) Start(ctx context.Context) error {
	if err := s.dirCreator(s.config.OutputDirectory); err != nil {
		return err
	}

	// Initialize progress bar if configured
	if s.config.ShowProgress {
		s.progress = progressbar.NewOptions(-1,
			progressbar.OptionSetDescription("Downloading"),
			progressbar.OptionShowCount(),
			progressbar.OptionSpinnerType(14),
			progressbar.OptionSetPredictTime(false),
			progressbar.OptionClearOnFinish(),
		)
	}

	if s.config.SkipExisting {
		s.logger.Warn("--skip-existing: cached pages cannot be re-parsed for child links (URLs are rewritten to local paths). Children of skipped pages will NOT be discovered. Re-run without --skip-existing for a full crawl.")
	}

	// Fetch robots.txt if configured
	if s.config.RespectRobots {
		s.fetchRobotsTxt(ctx)
	}

	if !s.shouldURLBeDownloaded(s.URL, 0, false) {
		return errors.New("start page is excluded from downloading")
	}

	// Announce the start URL as the root of the discovery tree so TUI
	// consumers can render it before its download begins.
	s.emit(Event{Kind: EventPageDiscovered, URL: s.URL.String(), Parent: "", Depth: 0})

	if err := s.processURL(ctx, s.URL, 0); err != nil {
		return err
	}

	head := 0
	for {
		s.webPageMu.Lock()
		if head >= len(s.webPageQueue) {
			s.webPageMu.Unlock()
			break
		}
		ur := s.webPageQueue[head]
		s.webPageQueue[head] = nil // release reference to allow GC
		urlStr := ur.String()
		currentDepth := s.webPageQueueDepth[urlStr]
		delete(s.webPageQueueDepth, urlStr)
		head++
		s.webPageMu.Unlock()

		if err := s.processURL(ctx, ur, currentDepth+1); err != nil && errors.Is(err, context.Canceled) {
			return err
		}
	}

	// Finish progress bar
	if s.progress != nil {
		_ = s.progress.Finish()
	}

	return nil
}

func (s *Scraper) processURL(ctx context.Context, u *url.URL, currentDepth uint) error {
	s.logger.Info("Downloading webpage", log.String("url", u.String()))
	s.emit(Event{Kind: EventPageStart, URL: u.String(), Depth: currentDepth})

	// Skip download if the target file already exists on disk.
	// Cached files have their URL references rewritten to local paths, so
	// re-parsing them would yield invalid URLs. We skip children entirely.
	if s.config.SkipExisting {
		filePath := s.getFilePath(u, true)
		if s.fileExistenceCheck(filePath) {
			s.logger.Debug("Skipping existing page",
				log.String("url", u.String()),
				log.String("file", filePath))
			s.incrementProgress()
			s.emit(Event{Kind: EventSkipped, URL: u.String(), Message: "existing on disk", Depth: currentDepth})
			return nil
		}
	}

	data, respURL, err := s.httpDownloader(ctx, u)
	if err != nil {
		// 403 on a web page is typically a permanent denial (auth-gated, special pages).
		// Record it and continue crawling instead of aborting — unless it is the
		// start page (depth 0), where a 403 means the entire scrape is invalid.
		if IsHTTPStatusError(err, http.StatusForbidden) {
			s.addFailedURL(u.String(), err)
			s.incrementProgress()
			if currentDepth == 0 {
				return fmt.Errorf("start page returned 403 Forbidden: %s", u.String())
			}
			s.logger.Warn("HTTP 403 on webpage, skipping",
				log.String("url", u.String()))
			s.emit(Event{Kind: EventFailed, URL: u.String(), Err: "403 Forbidden", Depth: currentDepth})
			return nil
		}
		s.logger.Error("Processing HTTP Request failed",
			log.String("url", u.String()),
			log.Err(err))
		s.emit(Event{Kind: EventFailed, URL: u.String(), Err: err.Error(), Depth: currentDepth})
		return err
	}

	fileExtension := ""
	kind, err := filetype.Match(data)
	if err == nil && kind != types.Unknown {
		fileExtension = kind.Extension
	}

	if currentDepth == 0 {
		// If the start page was redirected, the URL that emit'd the
		// EventPageDiscovered/Start events differs from the URL that
		// will own the children. Announce the redirected URL as
		// discovered so the TUI tree threads correctly: child pages
		// emit Parent = u.String() against the post-redirect URL.
		if respURL != nil && respURL.String() != u.String() {
			s.emit(Event{
				Kind:    EventPageDiscovered,
				URL:     respURL.String(),
				Parent:  u.String(),
				Depth:   0,
				Message: "redirected from start URL",
			})
		}
		u = respURL
		// use the URL that the website returned as new base url for the
		// scrape, in case of a redirect it changed
		s.URL = u
	}

	buf := bytes.NewBuffer(data)
	doc, err := html.Parse(buf)
	if err != nil {
		s.logger.Error("Parsing HTML failed",
			log.String("url", u.String()),
			log.Err(err))
		return fmt.Errorf("parsing HTML: %w", err)
	}

	index := htmlindex.New(s.logger)
	index.Index(u, doc)

	s.storeDownload(u, data, doc, index, fileExtension)

	if err := s.downloadReferences(ctx, index); err != nil {
		return err
	}

	// check first and download afterward to not hit max depth limit for
	// start page links because of recursive linking
	// a hrefs
	references, err := index.URLs(htmlindex.ATag)
	if err != nil {
		s.logger.Error("Parsing URL failed", log.Err(err))
	}

	queued := false
	for _, ur := range references {
		ur.Fragment = ""

		if s.shouldURLBeDownloaded(ur, currentDepth, false) {
			s.webPageMu.Lock()
			s.webPageQueue = append(s.webPageQueue, ur)
			s.webPageQueueDepth[ur.String()] = currentDepth
			s.webPageMu.Unlock()
			queued = true
			// Record the discovery edge so the TUI can render the
			// crawl as a tree. Depth is parent+1 (matches dequeue logic).
			s.emit(Event{
				Kind:   EventPageDiscovered,
				URL:    ur.String(),
				Parent: u.String(),
				Depth:  currentDepth + 1,
			})
		}
	}

	s.emit(Event{Kind: EventPageDone, URL: u.String(), Depth: currentDepth})
	if queued {
		s.emitQueueChanged()
	}
	return nil
}

// storeDownload writes the download to a file, if a known binary file is detected,
// processing of the file as page to look for links is skipped.
func (s *Scraper) storeDownload(u *url.URL, data []byte, doc *html.Node,
	index *htmlindex.Index, fileExtension string) {

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
	// always update html files, content might have changed
	if err := s.fileWriter(filePath, data); err != nil {
		s.logger.Error("Writing to file failed",
			log.String("URL", u.String()),
			log.String("file", filePath),
			log.Err(err))
	}
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
