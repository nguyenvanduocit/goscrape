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

// shouldURLBeDownloaded returns true when the URL passes every eligibility
// filter AND has not been seen before. Marking the URL as processed only
// happens after eligibility succeeds, so URLs rejected by filters/robots/depth
// stay un-marked and can pass on a later (legitimate) discovery.
func (s *Scraper) shouldURLBeDownloaded(u *url.URL, currentDepth uint, isAsset bool) bool {
	if !s.isURLEligible(u, currentDepth, isAsset) {
		return false
	}
	if !s.markAsProcessed(u) {
		return false
	}
	s.logger.Debug("New URL to download", log.String("url", u.String()))
	return true
}

// isURLEligible runs every pure eligibility check WITHOUT touching the
// processed set. Safe to call multiple times for the same URL.
func (s *Scraper) isURLEligible(u *url.URL, currentDepth uint, isAsset bool) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if !s.isHostEligible(u, isAsset) {
		return false
	}
	if !isAsset && s.isDepthExceeded(currentDepth) {
		s.logger.Debug("Skipping too deep level page", log.String("url", u.String()))
		return false
	}
	if !s.passesUserFilters(u) {
		return false
	}
	if !s.config.DisableDefaultExcludes && s.isURLDefaultExcluded(u) {
		s.logger.Debug("Skipping default-excluded URL", log.String("url", u.String()))
		return false
	}
	if !s.isRobotsAllowed(u) {
		s.logger.Debug("Blocked by robots.txt", log.String("url", u.String()))
		return false
	}
	return true
}

// isHostEligible rejects external pages always, and external assets when
// neither --include-external nor an allowed-CDN match applies.
func (s *Scraper) isHostEligible(u *url.URL, isAsset bool) bool {
	if u.Host == s.URL.Host {
		return true
	}
	if !isAsset {
		s.logger.Debug("Skipping external host page", log.String("url", u.String()))
		return false
	}
	if s.config.SkipExternalResources && !s.allowedCDN.Contains(u.Host) {
		s.logger.Debug("Skipping external asset", log.String("url", u.String()))
		return false
	}
	return true
}

// isDepthExceeded reports whether a page at currentDepth would exceed the
// configured MaxDepth. MaxDepth == 0 means unlimited.
func (s *Scraper) isDepthExceeded(currentDepth uint) bool {
	return s.config.MaxDepth != 0 && currentDepth == s.config.MaxDepth
}

// passesUserFilters applies user-supplied --include/--exclude regexes.
func (s *Scraper) passesUserFilters(u *url.URL) bool {
	if s.includes != nil && !s.isURLIncluded(u) {
		return false
	}
	if s.excludes != nil && s.isURLExcluded(u) {
		return false
	}
	return true
}

// isRobotsAllowed consults the loaded robots.txt (same-host only). Returns
// true when robots is disabled, not loaded, or the rule allows the URL.
func (s *Scraper) isRobotsAllowed(u *url.URL) bool {
	if s.robotsData == nil || u.Host != s.URL.Host {
		return true
	}
	agent := s.config.UserAgent
	if agent == "" {
		agent = "*"
	}
	return s.robotsData.TestAgent(u.Path, agent)
}

// processedKey computes the dedup key for a URL. Same-host URLs key on
// path (so URLs that differ only in their query string collapse to one
// entry); external URLs key on the full string. Trailing slash is
// normalized so "/foo" and "/foo/" map to one key.
//
// When Config.IncludeQueryInPath is set, a canonical-sorted query hash
// is folded into the key so /article?id=1 vs ?id=2 are tracked separately.
//
// Used by markAsProcessed and unmarkURLProcessed so both sides stay in sync.
func (s *Scraper) processedKey(u *url.URL) string {
	p := u.String()
	if u.Host == s.URL.Host {
		p = u.Path
	}
	if p == "" {
		p = "/"
	}
	key := normalizeURLPath(p)
	if s.config.IncludeQueryInPath {
		if h := queryHash(u); h != "" {
			// Use NUL as the path/query separator so a path containing a
			// literal "%3F" (decoded "?") cannot collide with the synthetic
			// "<path>?<hash>" key for a different URL.
			key += "\x00q=" + h
		}
	}
	return key
}

// markAsProcessed atomically inserts the URL key into the processed set.
// Returns true if the URL was newly inserted; false if another call had
// already inserted it (in which case the caller must NOT proceed).
func (s *Scraper) markAsProcessed(u *url.URL) bool {
	key := s.processedKey(u)
	s.processedMu.Lock()
	defer s.processedMu.Unlock()
	if s.processed.Contains(key) {
		return false
	}
	s.processed.Add(key)
	return true
}

// unmarkURLProcessed removes a URL from the processed set so it can be
// retried by a later discovery. Use ONLY after a URL was accepted (i.e.
// passed eligibility + got marked) but then failed downstream (download
// error, write fail, dir-create fail). Do NOT call for URLs rejected by
// eligibility filters — those are never marked in the first place.
func (s *Scraper) unmarkURLProcessed(u *url.URL) {
	key := s.processedKey(u)
	s.processedMu.Lock()
	s.processed.Remove(key)
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
