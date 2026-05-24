// Package main provides a command line tool to scrape websites and create an offline browsable version on the disk.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/alexflint/go-arg"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/cornelk/goscrape/scraper"
	"github.com/cornelk/goscrape/scraper/tui"
	"github.com/cornelk/gotokit/app"
	"github.com/cornelk/gotokit/buildinfo"
	"github.com/cornelk/gotokit/env"
	"github.com/cornelk/gotokit/log"
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

type arguments struct {
	Include []string `arg:"-n,--include,separate" help:"only include URLs with PERL Regular Expressions support"`
	Exclude []string `arg:"-x,--exclude,separate" help:"exclude URLs with PERL Regular Expressions support"`
	Output  string   `arg:"-o,--output" help:"output directory to write files to"`
	URLs    []string `arg:"positional"`

	// Allowlist scraping: feed an exact set of URLs and optionally disable
	// link discovery so only those URLs (plus their page assets) are saved.
	URLFile  string `arg:"-f,--url-file" help:"read URLs to scrape from a file (one URL per line; blank lines and lines starting with # are ignored). Merged with positional URLs"`
	NoFollow bool   `arg:"--no-follow" help:"do not follow <a> links; download only the given URLs (positional and/or --url-file). Page assets are still downloaded"`

	Depth        int64 `arg:"-d,--depth" help:"download depth, 0 for unlimited" default:"10"`
	ImageQuality int64 `arg:"-i,--imagequality" help:"image quality, 0 to disable reencoding"`
	Timeout      int64 `arg:"-t,--timeout" help:"time limit in seconds for each HTTP request to connect and read the request body"`

	Serve      string `arg:"-s,--serve" help:"serve the website using a webserver"`
	ServerPort int16  `arg:"-r,--serverport" help:"port to use for the webserver" default:"8080"`

	CookieFile     string `arg:"-c,--cookiefile" help:"file containing the cookie content"`
	SaveCookieFile string `arg:"--savecookiefile" help:"file to save the cookie content"`

	Headers   []string `arg:"-h,--header" help:"HTTP header to use for scraping"`
	Proxy     string   `arg:"-p,--proxy" help:"proxy to use in format scheme://[user:password@]host:port (supports HTTP, HTTPS, SOCKS5 protocols)"`
	User      string   `arg:"-u,--user" help:"user[:password] to use for HTTP authentication"`
	UserAgent string   `arg:"-a,--useragent" help:"user agent to use for scraping"`

	Verbose  bool `arg:"-v,--verbose" help:"verbose output"`
	Markdown bool `arg:"-m,--markdown" help:"convert HTML pages to Markdown (.md) instead of HTML"`
	TUI      bool `arg:"--tui" help:"run with interactive TUI dashboard (disables progress bar and silences logs)"`

	IncludeExternal bool     `arg:"-e,--include-external" help:"include external resources from other domains"`
	AllowCDN        []string `arg:"--allow-cdn,separate" help:"allow external assets from specific CDN domains (e.g., cdn.example.com)"`

	// Concurrency and rate limiting
	Concurrency int     `arg:"-j,--concurrency" help:"number of concurrent asset downloads (0 = use profile)" default:"0"`
	RateLimit   float64 `arg:"--rate-limit" help:"max requests per second, 0 = use profile or unlimited" default:"0"`
	Delay       int64   `arg:"--delay" help:"milliseconds delay between requests (0 = use profile)" default:"0"`
	Profile     string  `arg:"--profile" help:"preset: polite (2/1rps/250ms), balanced (6/4rps/0ms), aggressive (16/0/0ms)" default:"polite"`

	// Progress and logging
	NoProgress   bool   `arg:"--no-progress" help:"disable progress bar"`
	ErrorLogFile string `arg:"--error-log" help:"file to log failed URLs"`

	// robots.txt
	RespectRobots bool `arg:"--respect-robots" help:"respect robots.txt rules"`

	// Skip 403
	Skip403 bool `arg:"--skip-403" help:"silently skip asset URLs that return 403 Forbidden instead of logging errors"`

	// Default excludes
	NoDefaultExcludes bool `arg:"--no-default-excludes" help:"disable built-in deny list for auth/special pages (login, signup, MediaWiki Special:*, etc.)"`

	// Skip existing
	SkipExisting bool `arg:"--skip-existing" help:"skip download if the target file already exists on disk; useful for resuming an interrupted crawl"`

	// Include query string in dedup key and on-disk filename. Off by default
	// because tracking/session params can explode crawl size; enable for sites
	// where ?id=1 vs ?id=2 are genuinely different content.
	IncludeQuery bool `arg:"--include-query" help:"distinguish URLs that differ only in query string (default: query is dropped from dedup key and filename)"`
}

func (arguments) Description() string {
	return "Scrape a website and create an offline browsable version on the disk.\n"
}

func (arguments) Version() string {
	return fmt.Sprintf("Version: %s\n", buildinfo.Version(version, commit, date))
}

func main() {
	args, err := readArguments()
	if err != nil {
		fmt.Printf("Reading arguments failed: %s\n", err)
		os.Exit(1)
	}

	ctx := app.Context()

	if args.Verbose {
		log.SetDefaultLevel(log.DebugLevel)
	}

	// In TUI mode any log line written to stderr/stdout would scramble the
	// rendered frame. Discard all log output and rely on the dashboard +
	// failed-URL log file for visibility.
	var logger *log.Logger
	if args.TUI {
		logger = log.NewNop()
	} else {
		var err error
		logger, err = createLogger()
		if err != nil {
			fmt.Printf("Creating logger failed: %s\n", err)
			os.Exit(1)
		}
	}

	if args.Serve != "" && args.Markdown {
		fmt.Println("--markdown is not supported with --serve")
		os.Exit(1)
	}

	if args.Serve != "" && args.TUI {
		fmt.Println("--tui is not supported with --serve")
		os.Exit(1)
	}

	if args.Serve != "" {
		if err := runServer(ctx, args, logger); err != nil {
			fmt.Printf("Server execution error: %s\n", err)
			os.Exit(1)
		}
		return
	}

	if err := runScraper(ctx, args, logger); err != nil {
		fmt.Printf("Scraping execution error: %s\n", err)
		os.Exit(1)
	}
}

func readArguments() (arguments, error) {
	var args arguments
	parser, err := arg.NewParser(arg.Config{}, &args)
	if err != nil {
		return arguments{}, fmt.Errorf("creating argument parser: %w", err)
	}

	if err = parser.Parse(os.Args[1:]); err != nil {
		switch {
		case errors.Is(err, arg.ErrHelp):
			parser.WriteHelp(os.Stdout)
			os.Exit(0)
		case errors.Is(err, arg.ErrVersion):
			fmt.Println(args.Version())
			os.Exit(0)
		}

		return arguments{}, fmt.Errorf("parsing arguments: %w", err)
	}

	// Validate BEFORE the URL-empty shortcut so "--depth -1" with no URL
	// still surfaces the input error instead of silently printing help.
	if err := validateArguments(&args); err != nil {
		return arguments{}, err
	}

	// Merge --url-file into the positional URLs before the empty-check so a
	// file-only invocation (no positional URLs) still proceeds to scrape.
	if err := mergeURLSources(&args); err != nil {
		return arguments{}, err
	}

	if len(args.URLs) == 0 && args.Serve == "" {
		parser.WriteHelp(os.Stdout)
		os.Exit(0)
	}

	return args, nil
}

// mergeURLSources reads URLs from --url-file (when set) and merges them with
// the positional URLs, de-duplicating while preserving first-seen order so a
// URL listed twice (or in both sources) is scraped once. Positional URLs come
// first, then file URLs.
func mergeURLSources(args *arguments) error {
	if args.URLFile == "" {
		return nil
	}
	fileURLs, err := readURLFile(args.URLFile)
	if err != nil {
		return err
	}
	args.URLs = dedupeStrings(append(args.URLs, fileURLs...))
	return nil
}

// readURLFile parses a URL list file: one URL per line, with surrounding
// whitespace trimmed. Blank lines and lines whose first non-space character
// is '#' are treated as comments and skipped.
func readURLFile(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading url file %q: %w", path, err)
	}
	var urls []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	return urls, nil
}

// dedupeStrings returns the input with duplicates removed, preserving the
// order of first occurrence.
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// Argument bounds — picked to catch obviously broken input without
// constraining legitimate use. Zero is allowed where it has well-defined
// semantics (timeout/delay 0 = none, depth 0 = unlimited, etc.).
const (
	maxDepth       = 1000
	maxTimeoutSecs = 86400 // 1 day
	maxDelayMillis = 60_000
	maxConcurrency = 1000
	maxRateLimit   = 1000.0
)

// validateArguments rejects negative numeric flags (which would silently
// cast to enormous uint values) and applies sanity caps so a typo can't
// turn into a multi-day sleep or runaway worker pool.
func validateArguments(args *arguments) error {
	type rangedInt struct {
		name     string
		value    int64
		min, max int64
		unit     string
	}
	for _, c := range []rangedInt{
		{"--depth", args.Depth, 0, maxDepth, ""},
		{"--timeout", args.Timeout, 0, maxTimeoutSecs, " seconds"},
		{"--delay", args.Delay, 0, maxDelayMillis, " milliseconds"},
		{"--concurrency", int64(args.Concurrency), 0, maxConcurrency, ""},
		{"--imagequality", args.ImageQuality, 0, 100, ""},
	} {
		if c.value < c.min || c.value > c.max {
			return fmt.Errorf("%s must be in [%d, %d]%s, got %d", c.name, c.min, c.max, c.unit, c.value)
		}
	}
	if math.IsNaN(args.RateLimit) || math.IsInf(args.RateLimit, 0) {
		return fmt.Errorf("--rate-limit must be a finite number, got %v", args.RateLimit)
	}
	if args.RateLimit < 0 || args.RateLimit > maxRateLimit {
		return fmt.Errorf("--rate-limit must be in [0, %.0f] req/s, got %g", maxRateLimit, args.RateLimit)
	}
	// int16 ServerPort already caps at 32767, but still reject negatives so
	// the message is actionable instead of "address already in use".
	if args.ServerPort < 0 {
		return fmt.Errorf("--serverport cannot be negative, got %d", args.ServerPort)
	}
	return nil
}

// applyProfile fills in zero-valued concurrency/rate/delay fields from the named profile.
// Fields explicitly set by the user (non-zero) are left unchanged.
func applyProfile(args *arguments) {
	type profileValues struct {
		concurrency int
		rateLimit   float64
		delay       int64
	}
	profiles := map[string]profileValues{
		"polite":     {concurrency: 2, rateLimit: 1.0, delay: 250},
		"balanced":   {concurrency: 6, rateLimit: 4.0, delay: 0},
		"aggressive": {concurrency: 16, rateLimit: 0, delay: 0},
	}
	p, ok := profiles[strings.ToLower(args.Profile)]
	if !ok {
		return // unknown profile, leave values as-is
	}
	if args.Concurrency == 0 {
		args.Concurrency = p.concurrency
	}
	if args.RateLimit == 0 {
		args.RateLimit = p.rateLimit
	}
	if args.Delay == 0 {
		args.Delay = p.delay
	}
}

func runScraper(ctx context.Context, args arguments, logger *log.Logger) error {
	if len(args.URLs) == 0 {
		return nil
	}

	applyProfile(&args)

	cookies, err := readCookieFile(args.CookieFile)
	if err != nil {
		return fmt.Errorf("reading cookie: %w", err)
	}

	cfg := buildScraperConfig(args, cookies)
	return scrapeURLs(ctx, cfg, logger, args)
}

// splitUserPassword splits "user[:password]" into its two parts. An empty
// input returns ("", "").
func splitUserPassword(user string) (string, string) {
	if user == "" {
		return "", ""
	}
	sl := strings.SplitN(user, ":", 2)
	if len(sl) == 1 {
		return sl[0], ""
	}
	return sl[0], sl[1]
}

// clampImageQuality keeps imagequality in [0, 100]; values outside that
// range disable re-encoding (treated as 0).
func clampImageQuality(q int64) uint {
	if q < 0 || q > 100 {
		return 0
	}
	return uint(q)
}

// buildScraperConfig assembles the scraper.Config from parsed CLI args.
// Kept separate from runScraper so the latter stays a simple orchestrator.
func buildScraperConfig(args arguments, cookies []scraper.Cookie) scraper.Config {
	username, password := splitUserPassword(args.User)
	return scraper.Config{
		Includes: args.Include,
		Excludes: args.Exclude,

		ImageQuality: clampImageQuality(args.ImageQuality),
		MaxDepth:     uint(args.Depth),
		Timeout:      uint(args.Timeout),

		OutputDirectory: args.Output,
		Username:        username,
		Password:        password,

		Cookies:   cookies,
		Header:    scraper.Headers(args.Headers),
		Proxy:     args.Proxy,
		UserAgent: args.UserAgent,

		SkipExternalResources: !args.IncludeExternal,
		AllowCDN:              args.AllowCDN,

		Concurrency: args.Concurrency,
		RateLimit:   args.RateLimit,
		Delay:       uint(args.Delay),

		// In TUI mode the dashboard owns the screen; the progressbar would
		// fight it for stdout, so force-disable.
		ShowProgress: !args.NoProgress && !args.TUI,
		ErrorLogFile: args.ErrorLogFile,

		RespectRobots:          args.RespectRobots,
		Skip403:                args.Skip403,
		DisableDefaultExcludes: args.NoDefaultExcludes,
		Markdown:               args.Markdown,
		SkipExisting:           args.SkipExisting,
		IncludeQueryInPath:     args.IncludeQuery,
		NoFollow:               args.NoFollow,
	}
}

func scrapeURLs(ctx context.Context, cfg scraper.Config,
	logger *log.Logger, args arguments) error {

	var failed int
	for _, url := range args.URLs {
		cfg.URL = url

		var runErr error
		if args.TUI {
			runErr = runWithTUI(ctx, cfg, logger, args)
		} else {
			runErr = runScrape(ctx, cfg, logger, args)
		}
		if runErr == nil {
			continue
		}
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			os.Exit(0)
		}
		// In allowlist mode (--no-follow) a URL list is best-effort: one bad
		// URL (403, network error, ...) must not abort the rest of the list,
		// since the whole point is to fetch every URL the caller listed.
		// Normal crawl mode keeps fail-fast — there a failing start URL means
		// the crawl can't proceed.
		if !args.NoFollow {
			return runErr
		}
		logger.Error("Skipping failed URL",
			log.String("url", url),
			log.Err(runErr))
		failed++
	}
	if failed > 0 {
		logger.Warn("Completed with failures",
			log.Int("failed", failed),
			log.Int("total", len(args.URLs)))
	}
	return nil
}

// runScrape executes a single scrape in plain (non-TUI) mode using the
// existing logger + optional progressbar path.
func runScrape(ctx context.Context, cfg scraper.Config,
	logger *log.Logger, args arguments) error {

	sc, err := scraper.New(logger, cfg)
	if err != nil {
		return fmt.Errorf("initializing scraper: %w", err)
	}

	logger.Info("Scraping", log.String("url", sc.URL.String()))
	if err = sc.Start(ctx); err != nil {
		return fmt.Errorf("scraping '%s': %w", sc.URL, err)
	}

	return finalizeScrape(sc, args)
}

// runWithTUI executes a single scrape with the Bubbletea dashboard.
// The scraper runs on a goroutine; the tea.Program owns the foreground
// thread. Events flow scraper → tea.Program.Send → Model.Update.
func runWithTUI(ctx context.Context, cfg scraper.Config,
	logger *log.Logger, args arguments) error {

	// Build the program first so we can route events into it before Start.
	model := tui.New(cfg.URL)
	program := tea.NewProgram(model, tea.WithAltScreen())

	handler, stopHandler := tui.NewEventHandler(program)
	cfg.OnEvent = handler

	sc, err := scraper.New(logger, cfg)
	if err != nil {
		stopHandler()
		return fmt.Errorf("initializing scraper: %w", err)
	}

	// Run the scraper in the background; surface completion to the TUI so
	// it can render a final frame and exit. Use a cancellable context so
	// pressing 'q' aborts an in-flight crawl instead of waiting for it.
	scrapeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var scrapeErr error
	doneCh := make(chan struct{})
	go func() {
		scrapeErr = sc.Start(scrapeCtx)
		program.Send(tui.DoneMsg{Err: scrapeErr})
		close(doneCh)
	}()

	// Run blocks until the TUI quits (user pressed q OR scraper finished
	// and the auto-quit timer fired).
	_, runErr := program.Run()
	cancel()      // ensure scraper goroutine notices if user quit early
	stopHandler() // prevent Send-after-Run
	<-doneCh      // wait for scraper to fully unwind before continuing

	if runErr != nil {
		return fmt.Errorf("running TUI: %w", runErr)
	}
	if scrapeErr != nil {
		if errors.Is(scrapeErr, context.Canceled) || errors.Is(scrapeErr, context.DeadlineExceeded) {
			// User aborted mid-flight; skip finalize so we don't write a
			// misleading success log/cookie snapshot.
			return nil
		}
		return fmt.Errorf("scraping '%s': %w", sc.URL, scrapeErr)
	}

	return finalizeScrape(sc, args)
}

// finalizeScrape persists cookies and failed-URL log after a scrape ends.
// Shared between TUI and plain mode so behaviour stays identical.
func finalizeScrape(sc *scraper.Scraper, args arguments) error {
	if args.SaveCookieFile != "" {
		if err := saveCookies(args.SaveCookieFile, sc.Cookies()); err != nil {
			return fmt.Errorf("saving cookies: %w", err)
		}
	}
	if args.ErrorLogFile != "" {
		if err := writeErrorLog(args.ErrorLogFile, sc.FailedURLs()); err != nil {
			return fmt.Errorf("writing error log: %w", err)
		}
	}
	return nil
}

func runServer(ctx context.Context, args arguments, logger *log.Logger) error {
	if err := scraper.ServeDirectory(ctx, args.Serve, args.ServerPort, logger); err != nil {
		return fmt.Errorf("serving directory: %w", err)
	}
	return nil
}

func createLogger() (*log.Logger, error) {
	logCfg, err := log.ConfigForEnv(env.Development)
	if err != nil {
		return nil, fmt.Errorf("initializing log config: %w", err)
	}
	logCfg.JSONOutput = false
	logCfg.CallerInfo = false

	logger, err := log.NewWithConfig(logCfg)
	if err != nil {
		return nil, fmt.Errorf("initializing logger: %w", err)
	}
	return logger, nil
}

func readCookieFile(cookieFile string) ([]scraper.Cookie, error) {
	if cookieFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(cookieFile)
	if err != nil {
		return nil, fmt.Errorf("reading cookie file: %w", err)
	}

	var cookies []scraper.Cookie
	if err := json.Unmarshal(b, &cookies); err != nil {
		return nil, fmt.Errorf("unmarshaling cookies: %w", err)
	}

	return cookies, nil
}

func saveCookies(cookieFile string, cookies []scraper.Cookie) error {
	if cookieFile == "" || len(cookies) == 0 {
		return nil
	}

	b, err := json.Marshal(cookies)
	if err != nil {
		return fmt.Errorf("marshaling cookies: %w", err)
	}

	if err := os.WriteFile(cookieFile, b, 0600); err != nil {
		return fmt.Errorf("saving cookies: %w", err)
	}

	return nil
}

func writeErrorLog(errorLogFile string, failedURLs []scraper.FailedURL) error {
	if len(failedURLs) == 0 {
		return nil
	}

	f, err := os.OpenFile(errorLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("opening error log file: %w", err)
	}
	defer func() { _ = f.Close() }()

	for _, failed := range failedURLs {
		line := fmt.Sprintf("%s\t%s\t%s\n", failed.Timestamp.Format("2006-01-02 15:04:05"), failed.URL, failed.Error)
		if _, err := f.WriteString(line); err != nil {
			return fmt.Errorf("writing to error log: %w", err)
		}
	}

	return nil
}
