# Codebase map: goscrape

**Audience:** Engineers adding features, debugging scraping failures, or performing security reviews.

**Primary tasks:**
1. Understand how a URL is discovered, filtered, downloaded, and saved.
2. Find the right file to modify when changing scraping behavior.
3. Assess blast radius before touching a shared component.

**Decision horizon:** Feature development, bug diagnosis, security review.

**Out of scope:** Deployment infrastructure (this is a CLI tool with no server component beyond the optional local `--serve` mode), performance profiling.

---

## Module map

| Module | Files | Purpose |
|---|---|---|
| [CLI / Entry](scraper-core.md#cli-entry-point) | `main.go` | Argument parsing, logger setup, cookie I/O, error log writing |
| [Scraper core](scraper-core.md) | `scraper/` (12 files) | URL queue, HTTP download, asset processing, file I/O |
| [HTML indexing](htmlindex-css.md) | `htmlindex/` (3 files) | Parse HTML, extract URLs by tag type |
| [CSS processing](htmlindex-css.md#css-processing) | `css/` (1 file) | Tokenize CSS, extract `url()` references |

---

## Architecture overview

```
┌────────────────────────────────────────────────────┐
│  CLI (main.go)                                     │
│  go-arg → Config → scraper.New() → scraper.Start() │
└─────────────────────────┬──────────────────────────┘
                          │
          ┌───────────────▼────────────────┐
          │  Scraper (scraper/scraper.go)   │
          │  BFS URL queue + concurrency    │
          └──┬──────────────┬──────────────┘
             │              │
   ┌─────────▼──────┐  ┌────▼──────────────────┐
   │  HTTP layer    │  │  Asset pipeline        │
   │  http.go       │  │  download.go           │
   │  retry, rate   │  │  CSS/JS URL rewriting  │
   │  limiting      │  │  image re-encoding     │
   └─────────┬──────┘  └────┬──────────────────┘
             │              │
   ┌─────────▼──────────────▼──────────────────┐
   │  HTML indexing (htmlindex/)                │
   │  Tag → attribute → URL extraction          │
   │  CSS tokenizing (css/)                     │
   └────────────────────────────────────────────┘
             │
   ┌─────────▼──────────────────────────────────┐
   │  File system (scraper/fs.go, fileutil.go)  │
   │  Path generation, traversal guard           │
   └────────────────────────────────────────────┘
```

---

## Key data flow

```
scraper.Start()
  → fetchRobotsTxt()               # optional
  → processURL(startURL, depth=0)
      → httpDownloader()           # download HTML with retries + rate limit
      → html.Parse()               # golang.org/x/net/html
      → htmlindex.Index()          # index URLs by tag type
      → storeDownload()            # rewrite HTML URL references, write file
      → downloadReferences()       # download CSS/JS/images concurrently
          → cssProcessor()         # rewrite CSS url() refs, enqueue discovered assets
          → jsProcessor()          # regex-extract JS asset refs
          → checkImageForRecode()  # re-encode JPEG/PNG if smaller
      → enqueue <a> href URLs      # add page links to BFS queue
  → BFS loop: repeat processURL() for each queued page
```

---

## Hub components (high coupling — high blast radius)

| Component | Location | Why it's a hub |
|---|---|---|
| `Scraper` struct | `scraper/scraper.go:118` | Holds all state; touched by every subsystem |
| `shouldURLBeDownloaded` | `scraper/checks.go:26` | All URL decisions pass through here |
| `resolveURL` | `scraper/url.go:13` | All URL rewriting in HTML, CSS, JS uses this function |
| `htmlindex.Index` | `htmlindex/htmlindex.go:16` | Bridge between HTML parsing and all asset processing |
| `getFilePath` | `scraper/fileutil.go:50` | Every file write uses this; path traversal guard lives here |

**Change any of these five carefully.** They affect every scraping path.

---

## Infrastructure context

goscrape is a self-contained CLI binary. No containers, no serverless, no queue systems.

- **Build**: `go install` or `goreleaser` (see `.goreleaser.yaml`)
- **Distribution**: GitHub releases via goreleaser
- **Testing**: `go test -race ./...` — race detector enabled
- **Linting**: `golangci-lint v2.6.0` (see `.golangci.yml`)
- **Docker**: `Dockerfile` present for containerized use

---

## Security considerations

Two active SSRF mitigations exist:

1. **Blocked IP ranges** (`scraper/scraper.go:79-96`): CGNAT, TEST-NET, broadcast, and link-local CIDRs blocked via `init()`.
2. **Custom `DialContext`** (`scraper/scraper.go:207-234`): Resolves hostnames and blocks private/loopback IPs before connecting; dials resolved IP directly to prevent DNS rebinding.

The main scrape target host bypasses the IP check (`scraper/scraper.go:213-215`). This is intentional: the user explicitly chose that target.

**Path traversal guard** in `getFilePath` (`scraper/fileutil.go:79-93`) verifies the resolved output path stays under the output root using `filepath.Abs`.

---

## Priority recommendations

1. **Test coverage gap**: `scraper/url.go` has no direct test file. `urlRelativeToOther` and `urlRelativeToRoot` are complex path-manipulation functions — add table-driven tests.
2. **`jsProcessor` reliability**: Regex-based JS URL extraction (`scraper/download.go:303-311`) produces false positives on minified bundles. The 512KB size cutoff (`scraper/download.go:389`) mitigates this but is a blunt instrument.
3. **BFS memory growth**: The `webPageQueue` slice retains head entries as `nil` after processing (`scraper/scraper.go:323`). For very large sites, this accumulates. Consider periodic compaction.
4. **Inline style URL re-encoding**: `fixInlineStyleURL` returns on first `style` attribute found (`scraper/html.go:176-211`), silently skipping any subsequent `style` attributes on the same element (unusual in practice but possible).

---

## Unknowns

| Unknown | Verification step |
|---|---|
| Metrics for largest sites scraped in practice | Check issues/discussions in the GitHub repository |
| Whether `--allow-cdn` interacts correctly with `--skip-external=false` | Add integration test covering CDN + include-external flag combination |
| Behavior when `OutputDirectory` is empty string on Windows | Check `filepath.Join` behavior with empty first segment |

---

<!-- ORACLE-META
Written by codebase-oracle | 2026-03-13
Data: direct source reading (23 files)
Audience: feature engineers, debuggers, security reviewers | Confidence: 90%
Unknowns: 3 items pending verification
-->
