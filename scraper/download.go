package scraper

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cornelk/goscrape/css"
	"github.com/cornelk/goscrape/htmlindex"
	"github.com/cornelk/gotokit/log"
	"github.com/cornelk/gotokit/set"
)

// assetProcessor is a processor of a downloaded asset that can transform
// a downloaded file content before it will be stored on disk.
type assetProcessor func(URL *url.URL, data []byte) []byte

var tagsWithReferences = []string{
	htmlindex.LinkTag,
	htmlindex.ScriptTag,
	htmlindex.StyleTag,
	htmlindex.InlineStyleTag,
}

// submitReferences collects all asset URLs from the parsed HTML index and
// submits them to the unified task queue. Workers process them concurrently
// alongside page tasks — there is no per-page wait.
func (s *Scraper) submitReferences(index *htmlindex.Index) {
	// Body background images + img tags share the image processor.
	for _, tag := range []string{htmlindex.BodyTag, htmlindex.ImgTag} {
		references, err := index.URLs(tag)
		if err != nil {
			s.logger.Error("Getting node URLs failed",
				log.String("node", tag),
				log.Err(err))
		}
		for _, u := range references {
			s.submitAsset(u, s.checkImageForRecode)
		}
	}

	// Skip CSS/JS downloads in markdown mode — only images/fonts/media are needed.
	if s.config.Markdown {
		return
	}
	for _, tag := range tagsWithReferences {
		references, err := index.URLs(tag)
		if err != nil {
			s.logger.Error("Getting node URLs failed",
				log.String("node", tag),
				log.Err(err))
		}

		var processor assetProcessor
		switch tag {
		case htmlindex.LinkTag, htmlindex.StyleTag, htmlindex.InlineStyleTag:
			processor = s.cssProcessor
		case htmlindex.ScriptTag:
			processor = s.jsProcessor
		}
		for _, ur := range references {
			s.submitAsset(ur, processor)
		}
	}
}

// submitAsset checks the URL for eligibility and submits it to the unified
// task queue. Dedup happens here (before enqueue) so duplicate references
// don't waste queue slots or worker dispatches.
func (s *Scraper) submitAsset(u *url.URL, processor assetProcessor) {
	u.Fragment = ""
	if !s.shouldURLBeDownloaded(u, 0, true) {
		return
	}
	s.queue.submit(task{
		kind:      taskAsset,
		url:       u,
		processor: processor,
	})
}

// downloadAsset downloads an asset if it does not exist on disk yet.
// Callers must pre-check eligibility via shouldURLBeDownloaded before
// submitting to the queue — the check-and-mark happens at submit time
// in submitAsset to avoid wasting queue slots on duplicates.
func (s *Scraper) downloadAsset(ctx context.Context, u *url.URL, processor assetProcessor) error {
	u.Fragment = ""
	urlFull := u.String()

	filePath := s.getFilePath(u, false)
	if s.fileExists(filePath) {
		s.incrementProgress()
		s.emit(Event{Kind: EventSkipped, URL: urlFull, Message: "asset exists on disk"})
		return nil
	}

	s.logger.Info("Downloading asset", log.String("url", urlFull))
	s.emit(Event{Kind: EventAssetStart, URL: urlFull})

	// When no processor is needed, stream directly to disk to avoid holding
	// the entire response body in memory. This keeps memory bounded regardless
	// of asset size (videos, large PDFs, etc.).
	if processor == nil {
		return s.streamAssetToDisk(ctx, u, urlFull, filePath)
	}
	return s.bufferAndProcessAsset(ctx, u, urlFull, filePath, processor)
}

// streamAssetToDisk handles the no-processor path: ensure dir, stream
// body to file, emit lifecycle events.
func (s *Scraper) streamAssetToDisk(ctx context.Context, u *url.URL, urlFull, filePath string) error {
	if err := s.dirCreator(filepath.Dir(filePath)); err != nil {
		s.logger.Error("Creating asset directory failed",
			log.String("url", urlFull),
			log.String("dir", filepath.Dir(filePath)),
			log.Err(err))
		s.addFailedURL(urlFull, err)
		s.emit(Event{Kind: EventFailed, URL: urlFull, Err: err.Error()})
		return fmt.Errorf("creating asset directory: %w", err)
	}
	if _, err := s.httpStreamer(ctx, u, filePath); err != nil {
		return s.handleAssetDownloadError(u, urlFull, err)
	}
	s.incrementProgress()
	s.emit(Event{Kind: EventAssetDone, URL: urlFull})
	return nil
}

// bufferAndProcessAsset handles the processor path: buffer response,
// transform via processor, write to disk, emit lifecycle events.
func (s *Scraper) bufferAndProcessAsset(ctx context.Context, u *url.URL, urlFull, filePath string, processor assetProcessor) error {
	data, _, err := s.httpDownloader(ctx, u)
	if err != nil {
		return s.handleAssetDownloadError(u, urlFull, err)
	}

	data = processor(u, data)

	if err = s.fileWriter(filePath, data); err != nil {
		s.logger.Error("Writing asset file failed",
			log.String("url", urlFull),
			log.String("file", filePath),
			log.Err(err))
	}

	s.incrementProgress()
	s.emit(Event{Kind: EventAssetDone, URL: urlFull})
	return nil
}

// handleAssetDownloadError dispatches an asset HTTP download failure:
// 403 is silently skipped under config.Skip403; other errors mark the URL
// unprocessed (so a later discovery can retry), log, and emit EventFailed.
func (s *Scraper) handleAssetDownloadError(u *url.URL, urlFull string, err error) error {
	if s.config.Skip403 && IsHTTPStatusError(err, http.StatusForbidden) {
		s.incrementProgress()
		s.emit(Event{Kind: EventSkipped, URL: urlFull, Message: "asset 403, skipped"})
		return nil
	}
	// Unmark so a later discovery of the same asset (e.g. referenced from
	// HTML and CSS) can retry instead of being deduped silently.
	s.unmarkURLProcessed(u)
	s.logger.Error("Downloading asset failed",
		log.String("url", urlFull),
		log.Err(err))
	s.addFailedURL(urlFull, err)
	s.emit(Event{Kind: EventFailed, URL: urlFull, Err: err.Error()})
	return fmt.Errorf("downloading asset: %w", err)
}

func (s *Scraper) cssProcessor(baseURL *url.URL, data []byte) []byte {
	urls := make(map[string]string)
	var discovered []*url.URL

	// Calculate relative path prefix for external CSS files
	// For CSS at _external.com/path/file.css, we need to go up to website root
	var relativePrefix string
	if baseURL.Host != s.URL.Host {
		// External CSS: count directory levels in URL path (excluding filename)
		cssPath := baseURL.Path
		if cssPath == "" || cssPath == "/" {
			cssPath = "/"
		}
		// Count directory levels to go up: 1 for the file's directory + 1 for the _hostname directory
		pathDir := path.Dir(cssPath)
		levelsUp := 1
		if pathDir != "/" && pathDir != "." {
			levelsUp += strings.Count(pathDir, "/")
		}
		relativePrefix = strings.Repeat("../", levelsUp)
	}

	processor := func(token *css.Token, urlStr string, u *url.URL) {
		// Skip external URLs if configured
		if s.config.SkipExternalResources && u.Host != "" && u.Host != s.URL.Host {
			return
		}
		discovered = append(discovered, u)

		// Resolve URL relative to CSS file location (baseURL)
		resolved := resolveURL(baseURL, urlStr, s.URL.Host, false, "", s.config.SkipExternalResources, s.allowedCDN)

		// For external CSS files, prepend relative path to root
		if baseURL.Host != s.URL.Host && !strings.HasPrefix(resolved, "../") {
			resolved = relativePrefix + resolved
		}

		// token.Value is already "url(...)", so we map it to a new url('...')
		urls[token.Value] = "url('" + resolved + "')"
	}

	cssData := string(data)
	css.Process(s.logger, baseURL, cssData, processor)

	s.enqueueAssets(discovered)

	if len(urls) == 0 {
		return data
	}

	cssReplacerPairs := make([]string, 0, len(urls)*2)
	for ori, fixed := range urls {
		cssReplacerPairs = append(cssReplacerPairs, ori, fixed)
		s.logger.Debug("CSS Element relinked",
			log.String("url", ori),
			log.String("fixed_url", fixed))
	}
	if len(cssReplacerPairs) > 0 {
		cssData = strings.NewReplacer(cssReplacerPairs...).Replace(cssData)
	}

	return []byte(cssData)
}

// jsAssetExtensions lists file extensions to look for in JavaScript files.
const jsAssetExtensions = `png|jpg|jpeg|gif|svg|webp|ico|woff|woff2|ttf|eot|otf|css|js|json|mp4|webm|mp3|wav|ogg|pdf|riv|lottie|glb|gltf|wasm`

// Package-level compiled regexes for JS asset URL extraction.
var (
	jsLocalPathPattern    = regexp.MustCompile("[\"'`](/[^\"'`]*\\.(" + jsAssetExtensions + "))[\"'`]")
	jsExternalPathPattern = regexp.MustCompile("[\"'`](https?://[^\"'`]+\\.(" + jsAssetExtensions + "))[\"'`]")
	jsDirPathPattern      = regexp.MustCompile("[\"'`](/[a-zA-Z0-9_-]+(?:/[a-zA-Z0-9_-]+)*/)[\"'`]")
	jsFileNamePattern     = regexp.MustCompile("[\"'`]([a-zA-Z0-9_-]+\\.(" + jsAssetExtensions + "))[\"'`]")
	jsTemplateFilePattern = regexp.MustCompile(`\}([a-zA-Z0-9_-]+\.(` + jsAssetExtensions + `))`)
	cdnBasePattern        = regexp.MustCompile(`["'](https?://(unpkg\.com|cdn\.jsdelivr\.net|cdnjs\.cloudflare\.com)/?)["']`)
)

// findDirFileCombinations finds likely asset paths by combining directory paths and filenames
// found in JS code. Uses O(D+F) map lookup instead of O(D*F) nested loop.
func findDirFileCombinations(jsData string, baseURL *url.URL, mainHost string,
	urls map[string]string, skipExternal bool, allowedCDN set.Set[string]) []*url.URL {

	dirPaths := jsDirPathPattern.FindAllStringSubmatch(jsData, -1)
	fileNames := jsFileNamePattern.FindAllStringSubmatch(jsData, -1)

	// Also look for template literal patterns: ${var}filename.ext where filename follows }
	templateFileNames := jsTemplateFilePattern.FindAllStringSubmatch(jsData, -1)
	fileNames = append(fileNames, templateFileNames...)

	// Build map from base name to filenames for O(1) lookup
	filesByBase := make(map[string][]string, len(fileNames))
	for _, fileMatch := range fileNames {
		if len(fileMatch) < 2 {
			continue
		}
		fileName := fileMatch[1]
		fileBase := strings.TrimSuffix(fileName, path.Ext(fileName))
		filesByBase[fileBase] = append(filesByBase[fileBase], fileName)
	}

	var discovered []*url.URL

	for _, dirMatch := range dirPaths {
		if len(dirMatch) < 2 {
			continue
		}
		dir := dirMatch[1]
		dirBase := strings.TrimSuffix(path.Base(strings.TrimSuffix(dir, "/")), "/")

		matchingFiles, ok := filesByBase[dirBase]
		if !ok {
			continue
		}

		for _, fileName := range matchingFiles {
			fullPath := dir + fileName
			if _, exists := urls[fullPath]; exists || strings.HasPrefix(fullPath, "data:") {
				continue
			}
			u, err := baseURL.Parse(fullPath)
			if err != nil {
				continue
			}
			if skipExternal && u.Host != "" && u.Host != mainHost {
				continue
			}
			discovered = append(discovered, u)
			resolved := resolveURL(baseURL, fullPath, mainHost, false, "", skipExternal, allowedCDN)
			urls[`"`+fullPath+`"`] = `"` + resolved + `"`
			urls[`'`+fullPath+`'`] = `'` + resolved + `'`
		}
	}

	return discovered
}

// rewriteCDNBases rewrites CDN URL bases for dynamic URL construction when downloading external assets.
func (s *Scraper) rewriteCDNBases(jsData string) string {
	return cdnBasePattern.ReplaceAllStringFunc(jsData, func(match string) string {
		quote := match[0:1]
		urlPart := match[1 : len(match)-1]
		u, err := url.Parse(urlPart)
		if err != nil {
			return match
		}
		localPath := "/_" + sanitizeHostForPath(u.Host) + u.Path
		s.logger.Debug("JS CDN base relinked", log.String("url", urlPart), log.String("fixed_url", localPath))
		return quote + localPath + quote
	})
}

// maxJSProcessSize is the maximum JS file size (bytes) to process for URL extraction.
// Large minified bundles produce excessive false positives and slow regex scans.
const maxJSProcessSize = 2 * 1024 * 1024

// jsProcessor processes JavaScript files to extract and rewrite URLs.
func (s *Scraper) jsProcessor(baseURL *url.URL, data []byte) []byte {
	if len(data) > maxJSProcessSize {
		return data
	}

	jsData := string(data)
	urls := make(map[string]string)
	skipExternal := s.config.SkipExternalResources

	discovered := findDirFileCombinations(jsData, baseURL, s.URL.Host, urls, skipExternal, s.allowedCDN)
	discovered = append(discovered, s.extractJSPatternURLs(baseURL, jsData, urls, skipExternal)...)
	s.enqueueAssets(discovered)

	markJSMainBaseRewrites(jsData, urls, s.URL.Host)

	jsData = s.applyJSReplacements(jsData, urls)
	if !skipExternal {
		jsData = s.rewriteCDNBases(jsData)
	}
	return []byte(jsData)
}

// extractJSPatternURLs walks the local + external regex patterns over jsData,
// records discovered asset URLs into urls (src → resolved), and returns the
// matched URLs for downstream enqueueing.
func (s *Scraper) extractJSPatternURLs(baseURL *url.URL, jsData string, urls map[string]string, skipExternal bool) []*url.URL {
	var discovered []*url.URL
	for _, pattern := range []*regexp.Regexp{jsLocalPathPattern, jsExternalPathPattern} {
		for _, match := range pattern.FindAllStringSubmatch(jsData, -1) {
			if u, src, ok := s.classifyJSMatch(baseURL, match, urls, skipExternal); ok {
				discovered = append(discovered, u)
				urls[src] = resolveURL(baseURL, src, s.URL.Host, false, "", skipExternal, s.allowedCDN)
			}
		}
	}
	return discovered
}

// classifyJSMatch validates a single regex match: must have a capture
// group, not a data URI, not already mapped, parseable against baseURL,
// and (when skipExternal is set) on the main host.
func (s *Scraper) classifyJSMatch(baseURL *url.URL, match []string, urls map[string]string, skipExternal bool) (*url.URL, string, bool) {
	if len(match) < 2 {
		return nil, "", false
	}
	src := match[1]
	if strings.HasPrefix(src, "data:") || urls[src] != "" {
		return nil, "", false
	}
	u, err := baseURL.Parse(src)
	if err != nil {
		s.logger.Debug("Failed to parse URL in JS", log.String("url", src), log.Err(err))
		return nil, "", false
	}
	if skipExternal && u.Host != "" && u.Host != s.URL.Host {
		return nil, "", false
	}
	return u, src, true
}

// markJSMainBaseRewrites records same-domain absolute base URLs (e.g.
// `"https://www.example.com`) for replacement with their quoting char so
// JS that builds URLs dynamically falls back to relative paths.
//
// Both http and https are checked because the JS may use a different
// scheme than the page that loaded it.
func markJSMainBaseRewrites(jsData string, urls map[string]string, mainHost string) {
	for _, scheme := range []string{"https", "http"} {
		mainBase := scheme + "://" + mainHost
		for _, quote := range []string{`"`, `'`, "`"} {
			prefix := quote + mainBase
			if strings.Contains(jsData, prefix) {
				urls[prefix] = quote
			}
		}
	}
}

// applyJSReplacements collapses the urls map into a single NewReplacer
// pass over jsData. Pairs where the target equals the source are ignored.
func (s *Scraper) applyJSReplacements(jsData string, urls map[string]string) string {
	jsReplacerPairs := make([]string, 0, len(urls)*2)
	for ori, filePath := range urls {
		if ori == filePath {
			continue
		}
		jsReplacerPairs = append(jsReplacerPairs, ori, filePath)
		s.logger.Debug("JS URL relinked", log.String("url", ori), log.String("fixed_url", filePath))
	}
	if len(jsReplacerPairs) == 0 {
		return jsData
	}
	return strings.NewReplacer(jsReplacerPairs...).Replace(jsData)
}

// enqueueAssets submits discovered asset URLs to the unified task queue.
// Called from cssProcessor/jsProcessor when they discover new URLs in
// CSS/JS content. Each URL gets an appropriate processor based on its
// file extension.
func (s *Scraper) enqueueAssets(urls []*url.URL) {
	for _, u := range urls {
		var proc assetProcessor
		if strings.ToLower(path.Ext(u.Path)) == ".css" {
			proc = s.cssProcessor
		} else {
			proc = s.checkImageForRecode
		}
		s.submitAsset(u, proc)
	}
}
