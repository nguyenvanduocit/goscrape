package scraper

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
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
	s.imagesQueue = append(s.imagesQueue, references...)

	// Collect img tags
	references, err = index.URLs(htmlindex.ImgTag)
	if err != nil {
		s.logger.Error("Getting img node URLs failed", log.Err(err))
	}
	s.imagesQueue = append(s.imagesQueue, references...)

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
		if tag == htmlindex.LinkTag {
			processor = s.cssProcessor
		}
		for _, ur := range references {
			tasks = append(tasks, downloadTask{url: ur, processor: processor})
		}
	}

	// Add image tasks
	for _, image := range s.imagesQueue {
		tasks = append(tasks, downloadTask{url: image, processor: s.checkImageForRecode})
	}
	s.imagesQueue = nil

	// Process tasks
	if err := s.processTasks(ctx, tasks); err != nil {
		return err
	}

	return nil
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
	taskChan := make(chan downloadTask, len(tasks))
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

	processor := func(token *css.Token, data string, u *url.URL) {
		s.imagesQueue = append(s.imagesQueue, u)

		cssPath := *u
		cssPath.Path = path.Dir(cssPath.Path) + "/"
		resolved := resolveURL(&cssPath, data, s.URL.Host, false, "")
		urls[token.Value] = resolved
	}

	cssData := string(data)
	css.Process(s.logger, baseURL, cssData, processor)

	if len(urls) == 0 {
		return data
	}

	for ori, filePath := range urls {
		cssData = replaceCSSUrls(ori, filePath, cssData)
		s.logger.Debug("CSS Element relinked",
			log.String("url", ori),
			log.String("fixed_url", filePath))
	}

	return []byte(cssData)
}
