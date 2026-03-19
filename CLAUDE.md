# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build and Development Commands

```bash
# Run tests
make test                    # or: go test -timeout 10s -race ./...

# Run a single test
go test -v -run TestFunctionName ./scraper

# Lint code
make lint                    # or: golangci-lint run

# Install linters
make install-linters

# Build and install
make install                 # or: go install -buildvcs=false .

# Test coverage
make test-coverage           # generate coverage report
make test-coverage-web       # view coverage in browser
```

## Architecture

goscrape is a website scraper that downloads websites for offline browsing.

**Core packages:**

- `main.go` - CLI entry point using `go-arg` for argument parsing. Handles two modes: scraping (`runScraper`) and serving downloaded content (`runServer`).

- `scraper/` - Core scraping logic
  - `scraper.go` - Main `Scraper` struct with URL queue processing. Uses breadth-first traversal with configurable depth limiting.
  - `download.go` - HTTP downloading with retry logic
  - `html.go` - HTML processing and URL reference fixing
  - `url.go` - URL validation, normalization, and include/exclude filtering via regex
  - `images.go` - JPEG/PNG quality reduction for disk space savings
  - `server.go` - Local HTTP server for browsing downloaded content

- `htmlindex/` - HTML document indexing
  - Parses HTML and indexes all URLs by tag type (a, img, link, script, etc.)
  - `attributes.go` - Defines which HTML attributes contain URLs per tag

- `css/` - CSS URL extraction using `gorilla/css` scanner

**Key data flow:**
1. `Scraper.Start()` processes the initial URL
2. HTML is parsed and indexed by `htmlindex.Index`
3. Asset URLs (images, CSS, JS) are downloaded immediately
4. Page URLs (`<a>` tags) are queued for breadth-first processing
5. URL references in HTML are rewritten to local paths before saving

**Dependencies:**
- `cornelk/gotokit` - Internal toolkit for logging, HTTP client, proxy support
- `h2non/filetype` - MIME type detection for downloaded files
- `gorilla/css` - CSS tokenization for URL extraction
