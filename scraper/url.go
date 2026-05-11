package scraper

import (
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/cornelk/gotokit/set"
)

// nolint: cyclop
func resolveURL(base *url.URL, reference, mainPageHost string, isHyperlink bool, relativeToRoot string, skipExternal bool, allowedCDN set.Set[string], pageExt ...string) string {
	ext := PageExtension
	if len(pageExt) > 0 && pageExt[0] != "" {
		ext = pageExt[0]
	}
	ur, err := url.Parse(reference)
	if err != nil {
		return ""
	}

	var resolvedURL *url.URL
	if ur.Host != "" && ur.Host != mainPageHost {
		// Check if this external host is in the allowed CDN list
		isAllowedCDN := allowedCDN != nil && allowedCDN.Contains(ur.Host)

		if isHyperlink || (skipExternal && !isAllowedCDN) {
			// do not change links to external websites or external assets when skipping
			return reference
		}

		resolvedURL = base.ResolveReference(ur)
		resolvedURL.Path = filepath.Join("_"+sanitizeHostForPath(ur.Host), resolvedURL.Path)
	} else {
		if isHyperlink {
			ur.Path = getPageFilePathWithExt(ur, ext)
		}
		resolvedURL = base.ResolveReference(ur)
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

	// Handle CDN concatenation URLs (e.g., /path/??file1.js,file2.js)
	// where ?? is parsed as query string. Incorporate first filename into path.
	if strings.HasPrefix(resolvedURL.RawQuery, "?") {
		query := strings.TrimPrefix(resolvedURL.RawQuery, "?")
		if files := strings.SplitN(query, ",", 2); len(files) > 0 && files[0] != "" {
			resolvedURL.Path = path.Join(resolvedURL.Path, files[0])
		}
	}

	resolvedURL.RawQuery = "" // remove query string - files are saved without it
	resolvedURL.ForceQuery = false
	resolved := resolvedURL.Path
	if resolvedURL.Fragment != "" {
		resolved += "#" + resolvedURL.Fragment
	}

	if resolved == "" {
		resolved = "/" // website root
	} else {
		if resolved[0] == '/' && len(relativeToRoot) > 0 {
			resolved = relativeToRoot + resolved[1:]
		} else {
			resolved = relativeToRoot + resolved
		}
	}

	if isHyperlink {
		dirIndex := "index" + ext
		if resolved[len(resolved)-1] == '/' {
			resolved += dirIndex
		} else {
			l := strings.LastIndexByte(resolved, '/')
			if l != -1 && l < len(resolved) && resolved[l+1] == '#' {
				resolved = resolved[:l+1] + dirIndex + resolved[l+1:]
			}
		}
	}

	resolved = strings.TrimPrefix(resolved, "/")
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
