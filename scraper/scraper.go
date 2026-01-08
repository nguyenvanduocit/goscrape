package scraper

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

	// Concurrency and rate limiting
	Concurrency int     // number of concurrent asset downloads
	RateLimit   float64 // max requests per second, 0 for unlimited
	Delay       uint    // milliseconds delay between requests

	// Progress and logging
	ShowProgress bool   // show progress bar
	ErrorLogFile string // file to log failed URLs

	// robots.txt
	RespectRobots bool // respect robots.txt rules
}

type (
	httpDownloader     func(ctx context.Context, u *url.URL) ([]byte, *url.URL, error)
	dirCreator         func(path string) error
	fileExistenceCheck func(filePath string) bool
	fileWriter         func(filePath string, data []byte) error
)

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
	processed set.Set[string]

	imagesQueue       []*url.URL
	webPageQueue      []*url.URL
	webPageQueueDepth map[string]uint

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
}

// New creates a new Scraper instance.
// nolint: funlen
func New(logger *log.Logger, cfg Config) (*Scraper, error) {
	var errs []error

	u, err := url.Parse(cfg.URL)
	if err != nil {
		errs = append(errs, err)
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
		s.rateLimiter = rate.NewLimiter(rate.Limit(cfg.RateLimit), 1)
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

	// Fetch robots.txt if configured
	if s.config.RespectRobots {
		s.fetchRobotsTxt(ctx)
	}

	if !s.shouldURLBeDownloaded(s.URL, 0, false) {
		return errors.New("start page is excluded from downloading")
	}

	if err := s.processURL(ctx, s.URL, 0); err != nil {
		return err
	}

	for len(s.webPageQueue) > 0 {
		ur := s.webPageQueue[0]
		s.webPageQueue = s.webPageQueue[1:]
		currentDepth := s.webPageQueueDepth[ur.String()]

		if s.config.SkipExternalResources && ur.Host != s.URL.Host {
			s.logger.Debug("Skipping external resource", log.String("url", ur.String()))
			continue
		}

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
	data, respURL, err := s.httpDownloader(ctx, u)
	if err != nil {
		s.logger.Error("Processing HTTP Request failed",
			log.String("url", u.String()),
			log.Err(err))
		return err
	}

	fileExtension := ""
	kind, err := filetype.Match(data)
	if err == nil && kind != types.Unknown {
		fileExtension = kind.Extension
	}

	if currentDepth == 0 {
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

	for _, ur := range references {
		ur.Fragment = ""

		if s.shouldURLBeDownloaded(ur, currentDepth, false) {
			s.webPageQueue = append(s.webPageQueue, ur)
			s.webPageQueueDepth[ur.String()] = currentDepth
		}
	}

	return nil
}

// storeDownload writes the download to a file, if a known binary file is detected,
// processing of the file as page to look for links is skipped.
func (s *Scraper) storeDownload(u *url.URL, data []byte, doc *html.Node,
	index *htmlindex.Index, fileExtension string) {

	// We need to distinguish between HTML pages and binary files (images, PDFs, etc.)
	// because they need different file path handling:
	// - HTML pages: add .html extension, handle directory indexes like /about -> /about.html
	// - Binary files: keep original path, so /photo.jpg stays /photo.jpg, not /photo.jpg.html
	// This prevents breaking binary downloads that were working before.
	isAPage := false
	if fileExtension == "" {
		fixed, hasChanges, err := s.fixURLReferences(u, doc, index)
		if err != nil {
			s.logger.Error("Fixing file references failed",
				log.String("url", u.String()),
				log.Err(err))
			return
		}

		if hasChanges {
			data = fixed
		}
		// Only HTML content gets processed as a "page" - binary files stay as-is
		isAPage = true
	}

	filePath := s.getFilePath(u, isAPage)
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
func (s *Scraper) fetchRobotsTxt(ctx context.Context) {
	robotsURL := &url.URL{
		Scheme: s.URL.Scheme,
		Host:   s.URL.Host,
		Path:   "/robots.txt",
	}

	s.logger.Debug("Fetching robots.txt", log.String("url", robotsURL.String()))

	data, _, err := s.downloadURLWithRetries(ctx, robotsURL)
	if err != nil {
		s.logger.Debug("Failed to fetch robots.txt, ignoring", log.Err(err))
		return
	}

	robots, err := robotstxt.FromBytes(data)
	if err != nil {
		s.logger.Debug("Failed to parse robots.txt, ignoring", log.Err(err))
		return
	}

	s.robotsData = robots
	s.logger.Debug("Loaded robots.txt successfully")
}
