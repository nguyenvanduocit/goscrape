package scraper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"net/url"
	"path/filepath"
	"sort"
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

// queryHashLength is the number of hex characters from the SHA-256 query
// digest that gets appended to filenames. 16 hex chars = 64 bits — birthday
// collision starts mattering around 4 billion distinct queries, well past
// any reasonable crawl size.
const queryHashLength = 16

// canonicalQuery returns the query string in canonical (sorted key+value)
// form so that "?b=1&a=2" and "?a=2&b=1" map to the same hash. Returns ""
// when the URL has no real query. CDN concat ("??file,...") is handled by
// the caller before this is invoked.
func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	values := u.Query()
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		vs := values[k]
		sort.Strings(vs)
		for j, v := range vs {
			if i > 0 || j > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(v))
		}
	}
	return b.String()
}

// queryHash returns a short, stable hex digest of the canonical query so
// it can be appended to filenames without leaking unsafe characters. Returns
// "" when there is no query.
func queryHash(u *url.URL) string {
	q := canonicalQuery(u)
	if q == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:])[:queryHashLength]
}

// isCDNConcatQuery reports whether u.RawQuery is the "??file,..." form used
// by CDN concat URLs. These are folded into the path by other helpers and
// must NOT be treated as a normal query string.
func isCDNConcatQuery(u *url.URL) bool {
	return strings.HasPrefix(u.RawQuery, "?")
}

// appendQueryHashToFilename inserts a stable, sorted hash of the URL's
// query string into a filename before its extension. With ext: "x.js" +
// hash "abc" → "x-abc.js". Without ext: "x" → "x-abc". Returns fileName
// unchanged when there is no query (or it is a CDN concat).
func appendQueryHashToFilename(fileName string, u *url.URL) string {
	if isCDNConcatQuery(u) {
		return fileName
	}
	h := queryHash(u)
	if h == "" {
		return fileName
	}
	ext := filepath.Ext(fileName)
	base := strings.TrimSuffix(fileName, ext)
	return base + "-" + h + ext
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
	// If not a page, keep the original URL path for binary files.
	// Drop the leading slash so filepath.Join doesn't have to clean it
	// across platforms (Windows treats a leading slash as drive-rooted).
	fileName = strings.TrimPrefix(fileName, "/")

	// Opt-in: append a hash of the canonical query so URLs that differ only
	// in their query string (e.g. /article?id=1 vs ?id=2) get distinct files.
	if s.config.IncludeQueryInPath {
		fileName = appendQueryHashToFilename(fileName, url)
	}

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

	// Disambiguate case-insensitive collisions BEFORE the traversal guard so
	// the guard sees the final path.
	result = s.disambiguateCaseCollision(result, url)

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

// caseCollisionMaxAttempts caps the suffix-retry loop in
// disambiguateCaseCollision. Each attempt rotates the URL hash so distinct
// URLs land on distinct synthetic filenames; the cap protects against the
// vanishingly-rare event of repeated hash collisions on the candidate slot.
const caseCollisionMaxAttempts = 16

// disambiguateCaseCollision detects two URLs that produce filesystem paths
// differing ONLY in case (e.g. /Logo.png and /logo.png) and would clobber
// each other on case-insensitive filesystems (macOS, Windows). The first
// URL to claim a lowercased path keeps its original casing; later URLs
// whose path lowercases to the same value but uses different casing get a
// short hash inserted before the extension.
//
// Multiple URLs producing the IDENTICAL filepath (e.g. two URLs that differ
// only in query when --include-query is off) are NOT a case collision —
// they're an intentional same-target write and pass through unchanged.
//
// For the suffix-retry path the registry is checked atomically too so a
// literal URL that happens to match a previously-allocated synthetic path
// is still detected and forced to its own slot.
func (s *Scraper) disambiguateCaseCollision(result string, u *url.URL) string {
	identity := u.String()
	cleaned := filepath.Clean(result)
	lowerKey := strings.ToLower(cleaned)

	claim := casePathClaim{cased: cleaned, url: identity}
	if existing, loaded := s.casePathRegistry.LoadOrStore(lowerKey, claim); loaded {
		ex := existing.(casePathClaim)
		if ex.url == identity {
			return result // same URL re-asking — return the path it already got
		}
		// A natural claim with the same cased path is a legitimate same-target
		// write (e.g. query-drop). A synthetic claim that matches is a
		// literal-vs-synthetic coincidence and MUST be disambiguated.
		if !ex.synthetic && ex.cased == cleaned {
			return result
		}
		// Case-only collision OR literal-vs-synthetic match: fall through to
		// the suffix-allocation loop.
	} else {
		return result // first to claim this lowercased slot
	}

	for attempt := range caseCollisionMaxAttempts {
		candidate := insertHashBeforeExt(result, urlIdentityHashAttempt(identity, attempt))
		cleanedCand := filepath.Clean(candidate)
		lowerCand := strings.ToLower(cleanedCand)
		candClaim := casePathClaim{cased: cleanedCand, url: identity, synthetic: true}
		existing, loaded := s.casePathRegistry.LoadOrStore(lowerCand, candClaim)
		if !loaded {
			return candidate
		}
		if existing.(casePathClaim).url == identity {
			return candidate
		}
		// A different URL already owns this synthetic slot — rotate the hash
		// (attempt salt) and try again. We do NOT accept ex.cased == cleanedCand
		// here because both URLs would otherwise silently clobber on disk.
	}
	// Vanishingly rare: 16 consecutive hash collisions. Fall back to the
	// last candidate; better to risk an obscure clobber than to silently
	// drop a download.
	return insertHashBeforeExt(result, urlIdentityHashAttempt(identity, caseCollisionMaxAttempts-1))
}

// insertHashBeforeExt inserts "-<hash>" before the file extension. With
// extension: "logo.png" + "abc" → "logo-abc.png". Without: "page" → "page-abc".
func insertHashBeforeExt(path, hash string) string {
	dir, base := filepath.Split(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return filepath.Join(dir, stem+"-"+hash+ext)
}

// urlIdentityHashAttempt returns a short stable hex tag derived from the
// URL string plus an attempt salt. Attempt 0 hashes the URL alone; later
// attempts rotate the salt so colliding allocations get fresh candidates.
func urlIdentityHashAttempt(identity string, attempt int) string {
	const tagLen = 8
	var input string
	if attempt == 0 {
		input = identity
	} else {
		input = fmt.Sprintf("%s#%d", identity, attempt)
	}
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:tagLen]
}

// getPageFilePathWithExt returns a filename for a URL that represents a web page,
// using the specified file extension for pages without an existing extension.
// When pageExt is the markdown extension, any existing dynamic-page extension
// (.php, .aspx, .html, etc.) is replaced because the content stored on disk is
// markdown, not the original page type.
//
// The returned path preserves the URL's absoluteness: "/" → "/index.html",
// "/foo/" → "/foo/index.html". Callers that need a relative filesystem
// component should trim the leading slash themselves.
func getPageFilePathWithExt(url *url.URL, pageExt string) string {
	fileName := url.Path
	dirIndex := "index" + pageExt

	// root of domain will be index file
	switch {
	case fileName == "" || fileName == "/":
		fileName = "/" + dirIndex

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
