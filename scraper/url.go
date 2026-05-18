package scraper

import (
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/cornelk/gotokit/set"
)

func resolveURL(base *url.URL, reference, mainPageHost string, isHyperlink bool, relativeToRoot string, skipExternal bool, allowedCDN set.Set[string], pageExt ...string) string {
	ext := PageExtension
	if len(pageExt) > 0 && pageExt[0] != "" {
		ext = pageExt[0]
	}
	ur, err := url.Parse(reference)
	if err != nil {
		return ""
	}

	resolvedURL, keep := resolveAgainstBase(base, ur, mainPageHost, isHyperlink, skipExternal, allowedCDN, ext)
	if keep {
		return reference
	}

	if resolvedURL.Host == mainPageHost {
		resolvedURL.Path = urlRelativeToOther(resolvedURL, base, ext)
		relativeToRoot = ""
	}

	// Use the decoded path directly to match how files are saved on disk.
	// resolvedURL.String() re-encodes special characters (e.g., parentheses become %28/%29),
	// but getFilePath uses url.Path which is decoded. Using the decoded path ensures
	// HTML/CSS references match the actual filenames on disk.
	resolvedURL.Host = ""   // remove host
	resolvedURL.Scheme = "" // remove http/https

	applyCDNConcatQuery(resolvedURL)
	resolvedURL.RawQuery = "" // files are saved without query string
	resolvedURL.ForceQuery = false

	resolved := resolvedURL.Path
	if resolvedURL.Fragment != "" {
		resolved += "#" + resolvedURL.Fragment
	}
	resolved = applyRelativeToRoot(resolved, relativeToRoot)

	if isHyperlink {
		resolved = applyHyperlinkSuffix(resolved, ext)
	}
	return strings.TrimPrefix(resolved, "/")
}

// resolveAgainstBase classifies the reference (external vs same-host) and
// resolves it relative to base. Returns (resolvedURL, keepOriginal):
// keepOriginal=true means the caller should return the raw reference
// unchanged (external links or external assets when SkipExternalResources).
func resolveAgainstBase(base, ur *url.URL, mainPageHost string, isHyperlink, skipExternal bool, allowedCDN set.Set[string], ext string) (*url.URL, bool) {
	isExternal := ur.Host != "" && ur.Host != mainPageHost
	if isExternal {
		isAllowedCDN := allowedCDN != nil && allowedCDN.Contains(ur.Host)
		if isHyperlink || (skipExternal && !isAllowedCDN) {
			return nil, true
		}
		resolved := base.ResolveReference(ur)
		resolved.Path = filepath.Join("_"+sanitizeHostForPath(ur.Host), resolved.Path)
		return resolved, false
	}
	if isHyperlink {
		ur.Path = getPageFilePathWithExt(ur, ext)
	}
	return base.ResolveReference(ur), false
}

// applyCDNConcatQuery rewrites CDN concat URLs (e.g. "/path/??file1.js,file2.js")
// where "??" is parsed as a query string, by folding the first filename
// into the path so it maps cleanly onto disk.
func applyCDNConcatQuery(u *url.URL) {
	if !strings.HasPrefix(u.RawQuery, "?") {
		return
	}
	query := strings.TrimPrefix(u.RawQuery, "?")
	files := strings.SplitN(query, ",", 2)
	if len(files) == 0 || files[0] == "" {
		return
	}
	u.Path = path.Join(u.Path, files[0])
}

// applyRelativeToRoot prepends the relativeToRoot prefix to a resolved
// path, treating the empty path as the website root.
func applyRelativeToRoot(resolved, relativeToRoot string) string {
	if resolved == "" {
		return "/"
	}
	if resolved[0] == '/' && len(relativeToRoot) > 0 {
		return relativeToRoot + resolved[1:]
	}
	return relativeToRoot + resolved
}

// applyHyperlinkSuffix appends index<ext> when a hyperlink resolves to a
// directory or to a fragment alone, so the saved filename matches the
// on-disk page convention.
func applyHyperlinkSuffix(resolved, ext string) string {
	dirIndex := "index" + ext
	if resolved[len(resolved)-1] == '/' {
		return resolved + dirIndex
	}
	l := strings.LastIndexByte(resolved, '/')
	if l != -1 && l < len(resolved) && resolved[l+1] == '#' {
		return resolved[:l+1] + dirIndex + resolved[l+1:]
	}
	return resolved
}

func urlRelativeToRoot(url *url.URL) string {
	var rel strings.Builder
	splits := strings.Split(url.Path, "/")

	for i := range splits {
		if (len(splits[i]) > 0) && (i < len(splits)-1) {
			rel.WriteString("../")
		}
	}

	return rel.String()
}

func urlRelativeToOther(src, base *url.URL, pageExt ...string) string {
	ext := PageExtension
	if len(pageExt) > 0 && pageExt[0] != "" {
		ext = pageExt[0]
	}
	srcSplits := strings.Split(src.Path, "/")
	baseSplits := strings.Split(getPageFilePathWithExt(base, ext), "/")

	for len(srcSplits) > 0 && len(baseSplits) > 0 {
		if len(srcSplits[0]) == 0 {
			srcSplits = srcSplits[1:]
			continue
		}
		if len(baseSplits[0]) == 0 {
			baseSplits = baseSplits[1:]
			continue
		}

		if srcSplits[0] == baseSplits[0] {
			srcSplits = srcSplits[1:]
			baseSplits = baseSplits[1:]
		} else {
			break
		}
	}

	var upLevels strings.Builder

	for i, split := range baseSplits {
		if split == "" {
			continue
		}
		// Page filename is not a level.
		if i == len(baseSplits)-1 {
			break
		}
		upLevels.WriteString("../")
	}

	return upLevels.String() + path.Join(srcSplits...)
}
