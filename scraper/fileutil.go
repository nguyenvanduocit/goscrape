package scraper

import (
	"fmt"
	"hash/fnv"
	"net/url"
	"path/filepath"
	"strings"
)

// sanitizeHostForPath strips port and removes any characters that could cause
// path traversal when a hostname is used as a directory name.
func sanitizeHostForPath(host string) string {
	// Strip port
	if i := strings.LastIndex(host, ":"); i != -1 {
		host = host[:i]
	}
	// Strip brackets from IPv6 addresses
	host = strings.Trim(host, "[]")
	// Allowlist: only permit valid hostname characters
	var b strings.Builder
	for _, r := range host {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if isLetter || isDigit || r == '-' || r == '.' {
			b.WriteRune(r)
		}
	}
	host = b.String()
	host = strings.ReplaceAll(host, "..", "")
	if host == "" {
		host = "unknown"
	}
	return host
}

const (
	// PageExtension is the file extension that downloaded pages get.
	PageExtension = ".html"
	// PageDirIndex is the file name of the index file for every dir.
	PageDirIndex = "index" + PageExtension
	// MarkdownExtension is the file extension for markdown output.
	MarkdownExtension = ".md"
	// MaxFilenameLength is the maximum length for a filename component to ensure filesystem compatibility.
	MaxFilenameLength = 200
)

// pageExtension returns the file extension for HTML pages based on config.
func (s *Scraper) pageExtension() string {
	if s.config.Markdown {
		return MarkdownExtension
	}
	return PageExtension
}

// getFilePath returns a file path for a URL to store the URL content in.
// The isHTML parameter is crucial: it tells us whether this URL contains HTML content
// that should be treated as a web page (with .html extensions and directory indexing)
// or if it's a binary file that should keep its original path unchanged.
// Without this distinction, binary files would get corrupted paths like image.jpg.html.
func (s *Scraper) getFilePath(url *url.URL, isHTML bool) string {
	fileName := url.Path
	// Handle CDN concatenation URLs (e.g., /path/??file1.js,file2.js)
	// where ?? is parsed as query string, leaving path as a directory.
	if strings.HasPrefix(url.RawQuery, "?") {
		query := strings.TrimPrefix(url.RawQuery, "?")
		if files := strings.SplitN(query, ",", 2); len(files) > 0 && files[0] != "" {
			fileName = filepath.Join(fileName, files[0])
		}
	}
	if isHTML {
		// This is HTML content - apply web page naming conventions
		fileName = getPageFilePathWithExt(url, s.pageExtension())
	}
	// If not a page, keep the original URL path for binary files

	var externalHost string
	if url.Host != s.URL.Host {
		externalHost = "_" + sanitizeHostForPath(url.Host) // _ is a prefix for external domains on the filesystem
	}

	// Split the file path into directory and filename components
	dir := filepath.Dir(fileName)
	base := filepath.Base(fileName)

	// Truncate the filename component if it's too long
	truncatedBase := truncateFilename(base)

	// Reconstruct the path with the truncated filename
	if dir == "." {
		fileName = truncatedBase
	} else {
		fileName = filepath.Join(dir, truncatedBase)
	}

	result := filepath.Join(s.config.OutputDirectory, s.URL.Host, externalHost, fileName)

	// Guard against path traversal: ensure final path stays under the output root
	outputRoot := filepath.Join(s.config.OutputDirectory, s.URL.Host)
	absResult, err := filepath.Abs(result)
	if err != nil {
		return filepath.Join(outputRoot, "invalid_path")
	}
	absRoot, err := filepath.Abs(outputRoot)
	if err != nil {
		return filepath.Join(outputRoot, "invalid_path")
	}
	if !strings.HasPrefix(absResult, absRoot+string(filepath.Separator)) && absResult != absRoot {
		return filepath.Join(outputRoot, "invalid_path")
	}

	return result
}

// getPageFilePathWithExt returns a filename for a URL that represents a web page,
// using the specified file extension for pages without an existing extension.
// When pageExt is the markdown extension, any existing dynamic-page extension
// (.php, .aspx, .html, etc.) is replaced because the content stored on disk is
// markdown, not the original page type.
func getPageFilePathWithExt(url *url.URL, pageExt string) string {
	fileName := url.Path
	dirIndex := "index" + pageExt

	// root of domain will be index file
	switch {
	case fileName == "" || fileName == "/":
		fileName = dirIndex

	case fileName[len(fileName)-1] == '/':
		fileName += dirIndex

	default:
		ext := filepath.Ext(fileName)
		switch {
		case ext == "":
			fileName += pageExt
		case pageExt == MarkdownExtension:
			// Content has been converted to markdown — the file extension must
			// reflect the actual content type, not the URL's dynamic-page suffix.
			fileName = strings.TrimSuffix(fileName, ext) + pageExt
		}
	}

	return fileName
}

// truncateFilename truncates a filename if it exceeds MaxFilenameLength while preserving the extension.
func truncateFilename(filename string) string {
	if len(filename) <= MaxFilenameLength {
		return filename
	}

	ext := filepath.Ext(filename)
	baseName := strings.TrimSuffix(filename, ext)

	// Calculate how much space we need for hash and extension
	hashLength := 8 // Using first 8 hex characters (from 32-bit FNV)
	reservedLength := hashLength + len(ext)

	// If the extension alone is too long, truncate it too
	if reservedLength > MaxFilenameLength {
		ext = ext[:MaxFilenameLength-hashLength]
		reservedLength = hashLength + len(ext)
	}

	maxBaseLength := MaxFilenameLength - reservedLength
	if maxBaseLength <= 0 {
		maxBaseLength = 1
	}

	truncatedBase := baseName[:maxBaseLength]

	// Generate FNV-1a hash of original filename to ensure uniqueness
	h := fnv.New32a()
	_, _ = h.Write([]byte(filename))
	hashStr := fmt.Sprintf("%08x", h.Sum32())[:hashLength]

	return truncatedBase + hashStr + ext
}
