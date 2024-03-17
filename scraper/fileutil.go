package scraper

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/cornelk/gotokit/log"
)

const (
	// PageExtension is the file extension that downloaded pages get.
	PageExtension = ".html"
	// PageDirIndex is the file name of the index file for every dir.
	PageDirIndex = "index" + PageExtension
)

// getFilePath returns a file path for a URL to store the URL content in.
func (s *Scraper) getFilePath(url *url.URL, isAPage bool) string {
	fileName := url.Path
	if isAPage {
		fileName = getPageFilePath(url)
	}

	var externalHost string
	if url.Host != s.URL.Host {
		externalHost = "_" + url.Host // _ is a prefix for external domains on the filesystem
	}

	return filepath.Join(s.config.OutputDirectory, s.URL.Host, externalHost, fileName)
}

func (s *Scraper) writeFile(filePath string, buf *bytes.Buffer) error {
	dir := filepath.Dir(filePath)
	if len(dir) < len(s.URL.Host) { // nothing to append if it is the root dir
		dir = filepath.Join(".", s.URL.Host, dir)
	}

	s.logger.Debug("Creating dir", log.String("path", dir))
	err := os.MkdirAll(dir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("creating directory '%s': %w", dir, err)
	}

	s.logger.Debug("Creating file", log.String("path", filePath))
	f, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("creating file '%s': %w", filePath, err)
	}

	if _, err = f.Write(buf.Bytes()); err != nil {
		// nolint: wrapcheck
		_ = f.Close() // try to close and remove file but return the first error
		_ = os.Remove(filePath)
		return fmt.Errorf("writing to file: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}
	return nil
}

// getPageFilePath returns a filename for a URL that represents a page.
func getPageFilePath(url *url.URL) string {
	fileName := url.Path

	// root of domain will be index.html
	switch {
	case fileName == "" || fileName == "/":
		fileName = PageDirIndex
		// directory index will be index.html in the directory

	case fileName[len(fileName)-1] == '/':
		fileName += PageDirIndex

	default:
		ext := filepath.Ext(fileName)
		// if file extension is missing add .html, otherwise keep the existing file extension
		if ext == "" {
			fileName += PageExtension
		}
	}

	return fileName
}
