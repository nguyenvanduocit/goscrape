// Package scraper provides a web scraper that can download a website and its assets.
package scraper

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/cornelk/gotokit/log"
)

// defaultExcludePatterns contains compiled regexes for URLs that are almost never
// useful to scrape (auth pages, MediaWiki special pages, CMS admin endpoints).
// Compiled once at package init for zero per-request overhead.
var defaultExcludePatterns []*regexp.Regexp

func init() {
	rawPatterns := []string{
		`/wiki/Special:`,
		`(?i)[?&]title=Special:`,
		`(?i)/login(/|$|\?)`,
		`(?i)/logout(/|$|\?)`,
		`(?i)/signin(/|$|\?)`,
		`(?i)/signup(/|$|\?)`,
		`(?i)/register(/|$|\?)`,
		`(?i)[?&]action=(edit|delete|history|raw|submit|purge|info|protect|unprotect|watch|unwatch|rollback)`,
		`(?i)[?&]do=(login|register|logout)`,
		`(?i)/wp-login\.php`,
		`(?i)/wp-admin(/|$)`,
	}
	for _, p := range rawPatterns {
		defaultExcludePatterns = append(defaultExcludePatterns, regexp.MustCompile(p))
	}
}

// normalizeURLPath removes trailing slashes from URL paths for duplicate detection.
// This treats URLs with and without trailing slashes as the same resource.
func normalizeURLPath(path string) string {
	if path == "" {
		return "/"
	}
	// Keep root path as is, but remove trailing slashes from other paths
	if path != "/" && strings.HasSuffix(path, "/") {
		return strings.TrimSuffix(path, "/")
	}
	return path
}

// shouldURLBeDownloaded checks whether a page should be downloaded.
// nolint: cyclop,funlen
func (s *Scraper) shouldURLBeDownloaded(url *url.URL, currentDepth uint, isAsset bool) bool {
	if url.Scheme != "http" && url.Scheme != "https" {
		return false
	}

	p := url.String()
	if url.Host == s.URL.Host {
		p = url.Path
	}
	if p == "" {
		p = "/"
	}

	// Normalize the path for duplicate detection to handle trailing slashes
	normalizedPath := normalizeURLPath(p)

	// Atomically check-and-mark to prevent duplicate downloads
	s.processedMu.Lock()
	if s.processed.Contains(normalizedPath) {
		s.processedMu.Unlock()
		return false
	}
	s.processed.Add(normalizedPath)
	s.processedMu.Unlock()

	if url.Host != s.URL.Host {
		if !isAsset {
			s.logger.Debug("Skipping external host page", log.String("url", url.String()))
			return false
		}
		// Skip external assets by default, only download if --include-external is passed
		// or if the host is in the allowed CDN list
		if s.config.SkipExternalResources && !s.allowedCDN.Contains(url.Host) {
			s.logger.Debug("Skipping external asset", log.String("url", url.String()))
			return false
		}
	}

	if !isAsset {
		if s.config.MaxDepth != 0 && currentDepth == s.config.MaxDepth {
			s.logger.Debug("Skipping too deep level page", log.String("url", url.String()))
			return false
		}
	}

	if s.includes != nil && !s.isURLIncluded(url) {
		return false
	}
	if s.excludes != nil && s.isURLExcluded(url) {
		return false
	}

	if !s.config.DisableDefaultExcludes && s.isURLDefaultExcluded(url) {
		s.logger.Debug("Skipping default-excluded URL", log.String("url", url.String()))
		return false
	}

	// Check robots.txt rules if enabled (only for same domain)
	if s.robotsData != nil && url.Host == s.URL.Host {
		agent := s.config.UserAgent
		if agent == "" {
			agent = "*"
		}
		if !s.robotsData.TestAgent(url.Path, agent) {
			s.logger.Debug("Blocked by robots.txt", log.String("url", url.String()))
			return false
		}
	}

	s.logger.Debug("New URL to download", log.String("url", url.String()))
	return true
}

// unmarkURLProcessed removes a URL from the processed set, allowing it to be
// retried if it's discovered again (e.g., same asset referenced in both HTML and CSS).
func (s *Scraper) unmarkURLProcessed(u *url.URL) {
	p := u.String()
	if u.Host == s.URL.Host {
		p = u.Path
	}
	if p == "" {
		p = "/"
	}
	normalizedPath := normalizeURLPath(p)
	s.processedMu.Lock()
	s.processed.Remove(normalizedPath)
	s.processedMu.Unlock()
}

func (s *Scraper) isURLIncluded(url *url.URL) bool {
	for _, re := range s.includes {
		if re.MatchString(url.Path) {
			s.logger.Info("Including URL",
				log.String("url", url.String()),
				log.Stringer("included_expression", re))
			return true
		}
	}
	return false
}

func (s *Scraper) isURLExcluded(url *url.URL) bool {
	for _, re := range s.excludes {
		if re.MatchString(url.Path) {
			s.logger.Info("Skipping URL",
				log.String("url", url.String()),
				log.Stringer("excluded_expression", re))
			return true
		}
	}
	return false
}

// isURLDefaultExcluded checks the URL against the built-in deny list of
// auth/special page patterns that are almost never useful to scrape.
func (s *Scraper) isURLDefaultExcluded(u *url.URL) bool {
	testStr := u.Path
	if u.RawQuery != "" {
		testStr += "?" + u.RawQuery
	}
	for _, re := range defaultExcludePatterns {
		if re.MatchString(testStr) {
			return true
		}
	}
	return false
}
