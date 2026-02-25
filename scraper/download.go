package scraper

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/cornelk/goscrape/css"
	"github.com/cornelk/goscrape/htmlindex"
	"github.com/cornelk/gotokit/log"
)

// assetProcessor is a processor of a downloaded asset that can transform
// a downloaded file content before it will be stored on disk.
type assetProcessor func(URL *url.URL, data []byte) []byte

// downloadTask represents a single asset to download.
type downloadTask struct {
	url       *url.URL
	processor assetProcessor
}

var tagsWithReferences = []string{
	htmlindex.LinkTag,
	htmlindex.ScriptTag,
	htmlindex.BodyTag,
	htmlindex.StyleTag,
	htmlindex.InlineStyleTag,
}

func (s *Scraper) downloadReferences(ctx context.Context, index *htmlindex.Index) error {
	// Collect body background images
	references, err := index.URLs(htmlindex.BodyTag)
	if err != nil {
		s.logger.Error("Getting body node URLs failed", log.Err(err))
	}
	s.assetsMu.Lock()
	s.imagesQueue = append(s.imagesQueue, references...)
	s.assetsMu.Unlock()

	// Collect img tags
	references, err = index.URLs(htmlindex.ImgTag)
	if err != nil {
		s.logger.Error("Getting img node URLs failed", log.Err(err))
	}
	s.assetsMu.Lock()
	s.imagesQueue = append(s.imagesQueue, references...)
	s.assetsMu.Unlock()

	// Collect all tasks
	var tasks []downloadTask //nolint:prealloc

	for _, tag := range tagsWithReferences {
		references, err = index.URLs(tag)
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
			tasks = append(tasks, downloadTask{url: ur, processor: processor})
		}
	}

	// Add image tasks (snapshot under lock to avoid race with concurrent processors)
	s.assetsMu.Lock()
	pending := s.imagesQueue
	s.imagesQueue = nil
	s.assetsMu.Unlock()

	for _, image := range pending {
		tasks = append(tasks, downloadTask{url: image, processor: s.checkImageForRecode})
	}

	// Process tasks
	if err := s.processTasks(ctx, tasks); err != nil {
		return err
	}

	// Process any newly discovered assets from CSS/JS processing
	return s.processDiscoveredAssets(ctx)
}

// processDiscoveredAssets processes assets discovered during CSS/JS processing.
func (s *Scraper) processDiscoveredAssets(ctx context.Context) error {
	for {
		s.assetsMu.Lock()
		if len(s.imagesQueue) == 0 {
			s.assetsMu.Unlock()
			return nil
		}
		pending := s.imagesQueue
		s.imagesQueue = nil
		s.assetsMu.Unlock()

		var tasks []downloadTask
		for _, u := range pending {
			var proc assetProcessor
			if strings.ToLower(path.Ext(u.Path)) == ".css" {
				proc = s.cssProcessor
			} else {
				proc = s.checkImageForRecode
			}
			tasks = append(tasks, downloadTask{url: u, processor: proc})
		}
		if err := s.processTasks(ctx, tasks); err != nil {
			return err
		}
	}
}

// processTasks downloads assets either sequentially or concurrently based on config.
func (s *Scraper) processTasks(ctx context.Context, tasks []downloadTask) error {
	if len(tasks) == 0 {
		return nil
	}

	// Sequential download if concurrency is 1
	if s.config.Concurrency <= 1 {
		for _, task := range tasks {
			if err := s.downloadAsset(ctx, task.url, task.processor); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
			}
		}
		return nil
	}

	// Concurrent download with worker pool
	var wg sync.WaitGroup
	taskChan := make(chan downloadTask, s.config.Concurrency)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var cancelErr error
	var cancelOnce sync.Once

	// Start workers
	for range s.config.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskChan {
				if err := s.downloadAsset(ctx, task.url, task.processor); err != nil {
					if errors.Is(err, context.Canceled) {
						cancelOnce.Do(func() {
							cancelErr = err
							cancel()
						})
						return
					}
				}
			}
		}()
	}

	// Send tasks to workers
sendLoop:
	for _, task := range tasks {
		select {
		case <-ctx.Done():
			break sendLoop
		case taskChan <- task:
		}
	}
	close(taskChan)

	wg.Wait()

	if cancelErr != nil {
		return cancelErr
	}
	return nil
}

// downloadAsset downloads an asset if it does not exist on disk yet.
func (s *Scraper) downloadAsset(ctx context.Context, u *url.URL, processor assetProcessor) error {
	u.Fragment = ""
	urlFull := u.String()

	if !s.shouldURLBeDownloaded(u, 0, true) {
		return nil
	}

	filePath := s.getFilePath(u, false)
	if s.fileExists(filePath) {
		s.incrementProgress()
		return nil
	}

	s.logger.Info("Downloading asset", log.String("url", urlFull))
	data, _, err := s.httpDownloader(ctx, u)
	if err != nil {
		s.logger.Error("Downloading asset failed",
			log.String("url", urlFull),
			log.Err(err))
		s.addFailedURL(urlFull, err)
		return fmt.Errorf("downloading asset: %w", err)
	}

	if processor != nil {
		data = processor(u, data)
	}

	if err = s.fileWriter(filePath, data); err != nil {
		s.logger.Error("Writing asset file failed",
			log.String("url", urlFull),
			log.String("file", filePath),
			log.Err(err))
	}

	s.incrementProgress()
	return nil
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
		levelsUp := 2
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
		resolved := resolveURL(baseURL, urlStr, s.URL.Host, false, "", s.config.SkipExternalResources)

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

	for ori, fixed := range urls {
		// Direct replacement: token.Value -> url('new_path')
		cssData = strings.ReplaceAll(cssData, ori, fixed)
		s.logger.Debug("CSS Element relinked",
			log.String("url", ori),
			log.String("fixed_url", fixed))
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
	urls map[string]string, skipExternal bool) []*url.URL {

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
			resolved := resolveURL(baseURL, fullPath, mainHost, false, "", skipExternal)
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
const maxJSProcessSize = 512 * 1024

// jsProcessor processes JavaScript files to extract and rewrite URLs.
func (s *Scraper) jsProcessor(baseURL *url.URL, data []byte) []byte {
	if len(data) > maxJSProcessSize {
		return data
	}

	jsData := string(data)
	urls := make(map[string]string)
	var discovered []*url.URL
	skipExternal := s.config.SkipExternalResources

	patterns := []*regexp.Regexp{
		jsLocalPathPattern,
		jsExternalPathPattern,
	}

	discovered = append(discovered, findDirFileCombinations(jsData, baseURL, s.URL.Host, urls, skipExternal)...)

	for _, pattern := range patterns {
		for _, match := range pattern.FindAllStringSubmatch(jsData, -1) {
			if len(match) < 2 {
				continue
			}
			src := match[1]
			if strings.HasPrefix(src, "data:") || strings.Contains(src, "..") || urls[src] != "" {
				continue
			}
			u, err := baseURL.Parse(src)
			if err != nil {
				s.logger.Debug("Failed to parse URL in JS", log.String("url", src), log.Err(err))
				continue
			}
			if skipExternal && u.Host != "" && u.Host != s.URL.Host {
				continue
			}
			discovered = append(discovered, u)
			urls[src] = resolveURL(baseURL, src, s.URL.Host, false, "", skipExternal)
		}
	}

	s.enqueueAssets(discovered)

	for ori, filePath := range urls {
		if ori != filePath {
			jsData = strings.ReplaceAll(jsData, ori, filePath)
			s.logger.Debug("JS URL relinked", log.String("url", ori), log.String("fixed_url", filePath))
		}
	}

	if !skipExternal {
		jsData = s.rewriteCDNBases(jsData)
	}

	return []byte(jsData)
}

// enqueueAssets adds discovered asset URLs to the download queue (thread-safe).
func (s *Scraper) enqueueAssets(urls []*url.URL) {
	if len(urls) == 0 {
		return
	}
	s.assetsMu.Lock()
	s.imagesQueue = append(s.imagesQueue, urls...)
	s.assetsMu.Unlock()
}
