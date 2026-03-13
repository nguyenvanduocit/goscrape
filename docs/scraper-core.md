# Scraper core

**Audience:** Engineers adding features or debugging scraping failures.

**Primary tasks:**
1. Trace how a URL moves from discovery to a file on disk.
2. Understand concurrency boundaries and where locks are held.
3. Find where to add new asset type handling.

**Decision horizon:** Feature development, correctness bugs, security review.

**Out of scope:** HTML tag indexing (see [htmlindex-css.md](htmlindex-css.md)), CSS tokenization internals.

---

## CLI entry point

`main.go` parses arguments using `go-arg` and routes to one of two modes:

- **Scrape mode**: builds a `scraper.Config`, calls `scraper.New()` then `sc.Start()`.
- **Serve mode**: calls `scraper.ServeDirectory()` directly — no scraping involved.

The CLI exposes 20+ flags. Notable groups:

| Group | Flags | Config fields |
|---|---|---|
| Scope | `--depth`, `--include`, `--exclude` | `MaxDepth`, `Includes`, `Excludes` |
| Auth | `--user`, `--header`, `--cookiefile`, `--proxy` | `Username/Password`, `Header`, `Cookies`, `Proxy` |
| Concurrency | `--concurrency`, `--rate-limit`, `--delay` | `Concurrency`, `RateLimit`, `Delay` |
| Output control | `--imagequality`, `--no-progress`, `--error-log` | `ImageQuality`, `ShowProgress`, `ErrorLogFile` |
| Safety | `--respect-robots`, `--skip-403`, `--skip-external`, `--allow-cdn` | `RespectRobots`, `Skip403`, `SkipExternalResources`, `AllowCDN` |

Cookie files use JSON format (`[]scraper.Cookie`) — the same struct is used for both input (`--cookiefile`) and output (`--savecookiefile`). The cookie JSON schema is defined at `scraper/cookies.go:13`.

The error log (`--error-log`) appends tab-separated lines: `timestamp\turl\terror` (`main.go:303`).

---

## Scraper struct

`Scraper` (`scraper/scraper.go:118`) is the central data structure. All scraping state lives here.

Key fields:

```
Scraper {
  config          Config           // immutable after New()
  client          *http.Client     // shared HTTP client with cookie jar
  URL             *url.URL         // base URL, updated on redirect at depth 0
  includes        []*regexp.Regexp // compiled from Config.Includes
  excludes        []*regexp.Regexp // compiled from Config.Excludes

  processed       set.Set[string]  // visited URL paths (dedup)
  processedMu     sync.Mutex       // guards processed

  webPageQueue    []*url.URL       // BFS queue of HTML pages
  webPageQueueDepth map[string]uint // depth of each queued URL
  webPageMu       sync.Mutex       // guards queue + depth map

  assetQueue      []*url.URL       // pending asset downloads (CSS/JS may add during processing)
  assetsMu        sync.Mutex       // guards assetQueue

  dirCreator         func          // injectable: creates directories
  fileExistenceCheck func          // injectable: checks file existence
  fileWriter         func          // injectable: writes file
  httpDownloader     func          // injectable: HTTP GET with retries

  rateLimiter     *rate.Limiter    // nil when RateLimit == 0
  progress        *progressbar.ProgressBar
  doneURLs        int64            // atomic progress counter
  failedURLs      []FailedURL      // collected download failures
  failedURLsMu    sync.Mutex

  robotsData      *robotstxt.RobotsData  // nil when RespectRobots == false
  allowedCDN      set.Set[string]  // external domains allowed for assets
}
```

The 4 injectable function fields (`scraper/scraper.go:140-143`) are the testability mechanism. Tests replace them with in-memory stubs (`scraper/scraper_test.go:26-44`) to avoid touching the filesystem or network.

---

## SSRF protection

`New()` wraps the HTTP transport's `DialContext` (`scraper/scraper.go:207-234`) to block connections to private infrastructure:

1. Connections to the **main scrape target host** bypass all IP checks (the user explicitly provided this host).
2. For **all other hosts** (external assets, CDN, etc.), the dialer resolves the hostname and rejects any IP that is loopback, private, link-local, or in one of 7 additional blocked CIDR ranges (`scraper/scraper.go:83-96`): CGNAT, TEST-NET-1/2/3, reserved, broadcast.
3. After validation, the dialer connects to the **resolved IP directly** (`net.JoinHostPort(ips[0], port)`) to prevent DNS rebinding — the domain cannot resolve to a different IP mid-connection.

The blocked CIDR list is populated in `init()` (`scraper/scraper.go:83-96`) so it runs once at startup.

**Failure mode:** If a CDN hostname resolves to a private IP (misconfigured DNS), the connection is blocked and the asset download fails with `"blocked request to private/internal address"`. This is intentional.

---

## BFS traversal

`Start()` (`scraper/scraper.go:286`) implements breadth-first page traversal:

```
1. Create output directory
2. Initialize progress bar (if ShowProgress)
3. Fetch robots.txt (if RespectRobots)
4. Guard start URL against exclusion rules
5. processURL(startURL, depth=0)
6. Loop: dequeue from webPageQueue, call processURL(url, depth+1)
7. Finish progress bar
```

The queue uses a **head-index pattern** (`scraper/scraper.go:315-333`): `head` advances instead of popping, and dequeued slots are set to `nil` to release GC references. This avoids repeated slice reallocation but means the backing array grows monotonically for the lifetime of the scrape.

Depth tracking: each queued URL stores its depth in `webPageQueueDepth` (keyed by URL string). The depth increments by 1 per BFS level. `MaxDepth == 0` means unlimited.

`processURL()` (`scraper/scraper.go:343`) sequence:
1. HTTP download (with retries)
2. MIME type detection via `filetype.Match` — if unknown, treat as HTML
3. At depth 0: update `s.URL` to handle redirects
4. Parse HTML with `golang.org/x/net/html`
5. Build `htmlindex.Index`
6. `storeDownload()` — rewrite URLs, write file
7. `downloadReferences()` — download all assets
8. Extract `<a>` hrefs, enqueue those passing `shouldURLBeDownloaded`

---

## URL filtering (`checks.go`)

`shouldURLBeDownloaded()` (`scraper/checks.go:26`) decides whether a URL proceeds to download. It runs on every discovered URL. Decision order:

1. **Scheme check**: only `http` and `https`.
2. **Dedup check**: atomically check-and-add to `processed` set. External URLs use full URL string as key; same-host URLs use path only.
3. **External page check**: non-asset URLs from external hosts are always skipped.
4. **External asset check**: external assets skipped unless `--include-external` or URL host is in `allowedCDN`.
5. **Depth check**: skips pages (not assets) at or beyond `MaxDepth`.
6. **Include filter**: if `--include` patterns set, URL path must match at least one.
7. **Exclude filter**: if `--exclude` patterns set, URL path must not match any.
8. **robots.txt check**: only for same-domain URLs when `RespectRobots` is true.

**Design note:** The dedup check marks the URL as processed *before* the download attempt. A download failure does not remove the URL from `processed`, so failed URLs are not retried on the next BFS pass. Failed URLs are instead collected in `failedURLs` for the error log.

---

## HTTP layer (`http.go`)

`downloadURLWithRetries()` (`scraper/http.go:82`) wraps every download with:

1. **Rate limiting**: `rateLimiter.Wait(ctx)` blocks until the token bucket allows the request.
2. **Delay**: `app.Sleep(ctx, Delay)` adds a fixed delay before each request.
3. **Retry loop** (up to 10 attempts): retries on HTTP 403, 429, 500, 502, 503, 504 with linear backoff (`attempt * 1.5s`).
4. **Response body limit**: reads at most 100MB per response (`scraper/http.go:35`).

`downloadURL()` (`scraper/http.go:57`) sets `User-Agent`, `Authorization` (Basic auth), and any custom headers from `Config.Header`.

`HTTPStatusError` (`scraper/http.go:39`) is a typed error used by `--skip-403`: `downloadAsset()` checks `IsHTTPStatusError(err, 403)` (`scraper/download.go:210`) and silently skips the asset instead of logging.

**Failure mode:** Exhausting all 10 retries returns `errExhaustedRetries` and the URL is added to `failedURLs`. The scrape continues.

---

## Asset download pipeline (`download.go`)

`downloadReferences()` (`scraper/download.go:37`) collects all assets from the HTML index and dispatches them:

| Source | Tag | Processor |
|---|---|---|
| Body background | `<body background>` | `checkImageForRecode` |
| Images | `<img src/srcset>` | `checkImageForRecode` |
| Stylesheets | `<link href>`, `<style>`, inline `style=` | `cssProcessor` |
| Scripts | `<script src>` | `jsProcessor` |

After the initial batch, `processDiscoveredAssets()` (`scraper/download.go:99`) drains the `assetQueue` in a loop — CSS and JS processors enqueue additional assets (fonts, images referenced inside CSS/JS) via `enqueueAssets()`. The loop continues until the queue is empty.

### Concurrency model

When `Concurrency > 1`, `processTasks()` (`scraper/download.go:127`) creates a worker pool:
- A buffered channel `taskChan` of size `Concurrency` feeds workers.
- Each worker goroutine processes tasks until the channel closes.
- Context cancellation (user interrupt) propagates through the channel and a `cancelOnce` guard.
- The main goroutine waits for all workers via `sync.WaitGroup`.

When `Concurrency == 1`, tasks run sequentially in the calling goroutine.

**Important:** Page downloads (BFS) are always sequential. Only assets within a single page download concurrently.

### CSS processor

`cssProcessor()` (`scraper/download.go:236`) uses the `css` package to tokenize CSS and find `url()` references. For each found URL:
1. Skips external URLs when `SkipExternalResources` is true.
2. Resolves the URL relative to the CSS file's location (not the main page).
3. For CSS files hosted on external domains, prepends a `../` prefix sequence to reach the local root.
4. Rewrites the token in the CSS source using string replacement.
5. Enqueues the URL for download.

### JS processor

`jsProcessor()` (`scraper/download.go:392`) uses 6 compiled regexes to extract URLs from JavaScript:
- `jsLocalPathPattern`: absolute paths starting with `/`
- `jsExternalPathPattern`: full `https?://` URLs
- `jsDirPathPattern` + `jsFileNamePattern`: combined to infer `dir/file.ext` paths
- `jsTemplateFilePattern`: `}filename.ext` template literal patterns
- `cdnBasePattern`: CDN base URL rewriting (unpkg, jsdelivr, cdnjs)

Files over 512KB are skipped entirely (`scraper/download.go:393`) to avoid excessive false positives in minified bundles.

---

## HTML URL rewriting (`html.go`)

After parsing, `fixURLReferences()` (`scraper/html.go:28`) walks every indexed HTML node and rewrites URL attributes to relative local paths. This is what makes downloaded pages work offline.

The core function is `resolveURL()` (`scraper/url.go:13`):
- Same-host URLs → compute relative path from current page to target file
- External-host URLs (assets) → rewrite to `_hostname/path` local mirror path
- External hyperlinks → leave unchanged (user may not have downloaded them)
- `data:` and `mailto:` → leave unchanged

`urlRelativeToRoot()` (`scraper/url.go:72`) counts directory depth to compute how many `../` prefixes are needed.

`urlRelativeToOther()` (`scraper/url.go:85`) trims common path prefix between source and base URL, then constructs the relative path.

Inline `style` attributes are handled separately via `fixInlineStyleURL()` (`scraper/html.go:175`), which parses them as CSS snippets.

`srcset` attributes use `resolveSrcSetURLs()` (`scraper/html.go:215`), which splits on `,` and resolves each URL independently.

---

## File system (`fs.go`, `fileutil.go`)

`getFilePath()` (`scraper/fileutil.go:50`) maps a URL to a filesystem path:

1. HTML content → `getPageFilePath()`: adds `.html` extension, maps `/` → `index.html`, maps `/dir/` → `/dir/index.html`.
2. Binary content → keep original URL path unchanged.
3. External hosts → prefix `_hostname/` (sanitized by `sanitizeHostForPath()`).
4. Long filenames → truncate to 200 chars, append 8-char FNV-1a hash for uniqueness (`scraper/fileutil.go:123`).
5. **Path traversal guard**: `filepath.Abs` verifies the resolved path stays under the output root (`scraper/fileutil.go:79-93`). Returns `"invalid_path"` on violation.

`sanitizeHostForPath()` (`scraper/fileutil.go:13`) allowlists hostname characters (`[a-zA-Z0-9\-.]`), strips ports and IPv6 brackets, removes `..` sequences.

`writeFile()` (`scraper/fs.go:24`) creates the file, writes data, and removes the file on write error to avoid partial files.

---

## Image re-encoding (`images.go`)

`checkImageForRecode()` (`scraper/images.go:17`) re-encodes JPEG and PNG images when `ImageQuality > 0`:
- Uses `h2non/filetype` for MIME detection (magic bytes, not extension).
- Re-encodes with the configured quality setting.
- Saves the re-encoded version **only if it is smaller** than the original (`scraper/images.go:70`, `94`).
- Falls back to the original on any decode/encode error.

PNG re-encoding uses Go's standard `image/png` encoder, which applies lossless compression. Quality reduction for PNG means smaller files via better compression, not lossy compression.

---

## Local server (`server.go`)

`ServeDirectory()` (`scraper/server.go:21`) starts a `http.FileServer` on `127.0.0.1:{port}` (default 8080). It registers additional MIME types for extensions the OS may not know (`.asp` → `text/html`, `.wasm` → `application/wasm`).

The server binds to loopback only — it cannot be accessed from other machines.

Shutdown is graceful: it listens for context cancellation and calls `server.Shutdown()`.

---

## Failure modes and recovery

| Failure | What happens | Recovery |
|---|---|---|
| HTTP 403/429/5xx | Retry up to 10 times with linear backoff | If all retries fail, URL added to `failedURLs`; scrape continues |
| HTTP 403 with `--skip-403` | Silent skip, no error log entry | None needed |
| HTML parse error | Error logged, page not saved | None; scraping continues |
| URL reference fix error | Error logged, raw data written as fallback | Offline links for that page may not work |
| File write error | Error logged, no partial file left on disk | None; scraping continues |
| Image re-encode error | Original image saved | None |
| Private IP blocked | Download blocked, asset skipped | Intentional; indicates SSRF attempt or misconfigured DNS |
| robots.txt parse error | robots.txt ignored, scraping proceeds | Debug-level log only |

---

## Blast radius: safe change guide

| Change | Files affected | Risk | Test coverage |
|---|---|---|---|
| Add new HTML tag/attribute | `htmlindex/attributes.go` | Low — add entry to `Nodes` map | `htmlindex/htmlindex_test.go` |
| Add new asset type in `downloadReferences` | `scraper/download.go:37-96` | Medium — touches task dispatch | `scraper/scraper_test.go` |
| Change `resolveURL` | `scraper/url.go`, `scraper/html.go`, `scraper/download.go` | High — affects all offline link rewriting | `scraper/url_test.go`, `scraper/html_test.go` |
| Change `shouldURLBeDownloaded` | `scraper/checks.go` | High — affects all filtering logic | `scraper/checks_test.go` |
| Change `getFilePath` | `scraper/fileutil.go` | High — changes file layout on disk | `scraper/fileutil_test.go` |
| Change concurrency model | `scraper/download.go:127-189` | Medium — race conditions possible | Run with `-race` flag |
| Change SSRF protection | `scraper/scraper.go:207-234` | Critical — security boundary | Manual security review required |

---

## Unknowns

| Unknown | Verification step |
|---|---|
| Behavior of `jsProcessor` on real-world minified bundles (false positive rate) | Run scraper against a JS-heavy SPA and inspect rewritten URLs |
| Whether `assetQueue` can grow unboundedly from circular CSS `@import` chains | Trace `processDiscoveredAssets` loop with a CSS file that imports itself |
| Memory ceiling for `webPageQueue` on sites with 100k+ pages | Profile with `pprof` against a large site |

---

<!-- ORACLE-META
Written by codebase-oracle | 2026-03-13
Data: direct source reading of scraper/ (12 files) + main.go
Audience: feature engineers, debuggers | Confidence: 92%
Unknowns: 3 items pending verification
-->
