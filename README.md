# goscrape - create offline browsable copies of websites

[![Build status](https://github.com/cornelk/goscrape/actions/workflows/go.yaml/badge.svg?branch=main)](https://github.com/cornelk/goscrape/actions)
[![go.dev reference](https://img.shields.io/badge/go.dev-reference-007d9c?logo=go&logoColor=white&style=flat-square)](https://pkg.go.dev/github.com/cornelk/goscrape)
[![Go Report Card](https://goreportcard.com/badge/github.com/cornelk/goscrape)](https://goreportcard.com/report/github.com/cornelk/goscrape)
[![codecov](https://codecov.io/gh/cornelk/goscrape/branch/main/graph/badge.svg?token=NS5UY28V3A)](https://codecov.io/gh/cornelk/goscrape)

A web scraper written in Go. Download a website for offline reading or archival,
or convert pages to Markdown for note-taking and LLM workflows.

## Features

* Parallel crawler with a unified task queue and per-host rate limiting
* Three throughput presets (`polite`, `balanced`, `aggressive`) — sane defaults out of the box
* Optional interactive TUI dashboard showing discovery tree, in-flight assets, and live counters
* Markdown export mode — convert HTML pages to `.md` with YAML frontmatter
* `robots.txt` support, default deny list for auth/admin pages, regex include/exclude filters
* Resumable crawls via `--skip-existing` (skips files already on disk)
* JPEG and PNG re-encoding to reduce disk usage
* HTTP, HTTPS, and SOCKS5 proxies (with auth)
* HTTP basic auth, cookies (load + save), custom headers and user agent
* External-asset CDN allow-list (`--allow-cdn`) for sites that load fonts/images from third-party hosts
* Failed-URL error log (`--error-log`) for retry and audit
* Local HTTP server (`--serve`) to browse the downloaded copy
* SSRF guard: blocks requests to loopback / private / link-local / reserved IP ranges for non-target hosts

## Limitations

* Console only, no GUI version
* `--skip-existing` does not re-parse cached pages, so newly discovered links inside skipped pages are not followed

## Installation

Two options:

1. Download a binary from [Releases](https://github.com/cornelk/goscrape/releases)
2. Build from source (requires recent [Go](https://go.dev/)):

```
go install github.com/cornelk/goscrape@latest
```

## Usage

Scrape one or more sites:

```
goscrape https://example.com
goscrape https://site-a.com https://site-b.com
```

Serve a previously downloaded directory:

```
goscrape --serve example.com
```

Convert pages to Markdown instead of HTML:

```
goscrape --markdown https://example.com
```

Run with the interactive TUI dashboard:

```
goscrape --tui https://example.com
```

Resume an interrupted crawl (skip files already on disk):

```
goscrape --skip-existing https://example.com
```

Run with a higher-throughput profile and respect `robots.txt`:

```
goscrape --profile balanced --respect-robots https://example.com
```

## Throughput profiles

`--profile` picks defaults for concurrency, rate limiting, and inter-request delay.
Any value explicitly set on the command line overrides the profile.

| Profile      | Concurrency | Requests/sec/host | Delay  |
|--------------|-------------|--------------------|--------|
| `polite` (default) | 2     | 1                  | 250 ms |
| `balanced`   | 6           | 4                  | 0      |
| `aggressive` | 16          | unlimited          | 0      |

Tune individual parameters:

```
goscrape --concurrency 8 --rate-limit 5 --delay 100 https://example.com
```

## Options

```
  --include INCLUDE, -n INCLUDE
                         only include URLs matching a Perl-compatible regex (repeatable)
  --exclude EXCLUDE, -x EXCLUDE
                         exclude URLs matching a Perl-compatible regex (repeatable)
  --no-default-excludes  disable the built-in deny list for auth/special pages
                         (login, signup, MediaWiki Special:*, wp-admin, etc.)
  --output OUTPUT, -o OUTPUT
                         output directory to write files to
  --depth DEPTH, -d DEPTH
                         download depth, 0 for unlimited [default: 10]
  --imagequality N, -i N
                         JPEG/PNG quality (1-100), 0 to disable re-encoding
  --timeout T, -t T      per-request time limit in seconds
  --markdown, -m         convert HTML pages to Markdown (.md) with frontmatter
  --tui                  interactive TUI dashboard (silences logs and progress bar)
  --skip-existing        skip download if the target file already exists on disk
  --concurrency N, -j N  number of concurrent asset downloads (0 = use profile)
  --rate-limit R         max requests per second per host (0 = use profile)
  --delay MS             milliseconds delay between requests (0 = use profile)
  --profile P            polite (default) | balanced | aggressive
  --respect-robots       honor robots.txt rules for the target domain
  --skip-403             silently skip asset URLs that return 403 Forbidden
  --no-progress          disable the progress bar
  --error-log FILE       append failed URLs to FILE (tab-separated: time, URL, error)
  --include-external, -e download resources from external domains
  --allow-cdn HOST       allow external assets from specific hosts (repeatable)
  --serve DIR, -s DIR    serve DIR as a local website
  --serverport PORT, -r PORT
                         port for --serve [default: 8080]
  --cookiefile FILE, -c FILE
                         load cookies from FILE
  --savecookiefile FILE  write cookies to FILE after the run
  --header H, -h H       extra HTTP header (repeatable), e.g. "X-Token: abc"
  --proxy URL, -p URL    proxy URL (HTTP / HTTPS / SOCKS5 with optional user:password)
  --user U[:PASS], -u U  HTTP basic-auth credentials
  --useragent S, -a S    custom User-Agent string
  --verbose, -v          verbose output
  --help                 show this help and exit
  --version              show version and exit
```

## Cookies

Pass cookies via `--cookiefile` pointing at a JSON file in this format:

```json
[{"name":"user","value":"123"},{"name":"session","value":"sid"}]
```

Use `--savecookiefile` to persist any cookies set during the scrape.

## Proxy configuration

`--proxy` accepts HTTP, HTTPS, and SOCKS5 URLs with optional credentials:

```
# HTTP proxy
goscrape --proxy http://proxy.example.com:8080 https://example.com

# HTTPS proxy with auth
goscrape --proxy https://user:pass@proxy.example.com:8443 https://example.com

# SOCKS5 with auth
goscrape --proxy socks5://user:pass@proxy.example.com:1080 https://example.com
```

DNS resolution happens through the configured proxy where the protocol supports it,
and the SSRF guard still blocks resolved IPs in private / loopback / reserved ranges
for hosts other than the explicit scrape target.

## Markdown mode

`--markdown` converts each downloaded HTML page to Markdown with YAML frontmatter:

```markdown
---
title: "Page title from <title> tag"
url: "https://example.com/page"
scraped_at: "2026-05-18T12:34:56Z"
---

# Page content rendered as Markdown
```

In Markdown mode CSS and JS are skipped (images, fonts, media still download) and
dynamic-page suffixes such as `.php` / `.aspx` are rewritten to `.md` so the saved
filename matches the content.

## TUI dashboard

`--tui` renders an interactive Bubbletea dashboard showing:

* Active discovery path (root → current page) with ancestor lineage
* In-flight assets sorted by elapsed time
* Recently completed pages with status icons and duration
* Live page/asset/skipped counters and queue depth
* Per-event log of failures and skips

Press `q`, `Esc`, or `Ctrl+C` to quit. The dashboard owns the screen, so logs are
suppressed and `--error-log` is the recommended way to capture failures during a TUI run.
