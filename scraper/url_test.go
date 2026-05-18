package scraper

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveURL(t *testing.T) {
	type filePathFixture struct {
		BaseURL        url.URL
		Reference      string
		IsHyperlink    bool
		RelativeToRoot string
		Resolved       string
	}

	pathlessURL := url.URL{
		Scheme: "https",
		Host:   "petpic.xyz",
		Path:   "",
	}

	URL := url.URL{
		Scheme: "https",
		Host:   "petpic.xyz",
		Path:   "/earth/",
	}

	// Page one level below root, used to verify links to root resolve
	// relatively to the on-disk root index, not to a sibling.
	subdirPage := url.URL{
		Scheme: "https",
		Host:   "petpic.xyz",
		Path:   "/dir/page",
	}
	// Page two levels deep, exercises multi-level "../" computation.
	deeperPage := url.URL{
		Scheme: "https",
		Host:   "petpic.xyz",
		Path:   "/earth/brasil/rio",
	}

	var fixtures = []filePathFixture{
		{pathlessURL, "", true, "", "index.html"},
		{pathlessURL, "#contents", true, "", "#contents"},
		{URL, "brasil/index.html", true, "", "brasil/index.html"},
		{URL, "brasil/rio/index.html", true, "", "brasil/rio/index.html"},
		{URL, "../argentina/cat.jpg", false, "", "../argentina/cat.jpg"},
		// Task #4 regression: root href "/" from a subdir page must resolve
		// to the on-disk root index, NOT to a sibling under the current dir.
		{subdirPage, "/", true, "", "../index.html"},
		// Query is intentionally dropped from the on-disk path; the link
		// still points at the rewritten root index file.
		{subdirPage, "/?q=1", true, "", "../index.html"},
		// Multi-level depth: page at /earth/brasil/rio → root needs "../../".
		{deeperPage, "/", true, "", "../../index.html"},
	}

	for _, fix := range fixtures {
		resolved := resolveURL(&fix.BaseURL, fix.Reference, URL.Host, fix.IsHyperlink, fix.RelativeToRoot, false, nil)
		assert.Equal(t, fix.Resolved, resolved)
	}
}

func Test_urlRelativeToOther(t *testing.T) {
	type filePathFixture struct {
		SrcURL          url.URL
		BaseURL         url.URL
		ExpectedSrcPath string
	}

	var fixtures = []filePathFixture{
		{url.URL{Path: "/earth/brasil/rio/cat.jpg"}, url.URL{Path: "/earth/brasil/rio/"}, "cat.jpg"},
		{url.URL{Path: "/earth/brasil/rio/cat.jpg"}, url.URL{Path: "/earth/"}, "brasil/rio/cat.jpg"},
		{url.URL{Path: "/earth/cat.jpg"}, url.URL{Path: "/earth/brasil/rio/"}, "../../cat.jpg"},
		{url.URL{Path: "/earth/argentina/cat.jpg"}, url.URL{Path: "/earth/brasil/rio/"}, "../../argentina/cat.jpg"},
		{url.URL{Path: "/earth/brasil/rio/cat.jpg"}, url.URL{Path: "/mars/dogtown/"}, "../../earth/brasil/rio/cat.jpg"},
		{url.URL{Path: "///earth//////cat.jpg"}, url.URL{Path: "///earth/brasil//rio////////"}, "../../cat.jpg"},
	}

	for _, fix := range fixtures {
		relativeURL := urlRelativeToOther(&fix.SrcURL, &fix.BaseURL)
		assert.Equal(t, fix.ExpectedSrcPath, relativeURL)
	}
}

func Test_urlRelativeToRoot(t *testing.T) {
	type urlFixture struct {
		SrcURL   url.URL
		Expected string
	}

	var fixtures = []urlFixture{
		{url.URL{Path: "/earth/brasil/rio/cat.jpg"}, "../../../"},
		{url.URL{Path: "cat.jpg"}, ""},
		{url.URL{Path: "/earth/argentina"}, "../"},
		{url.URL{Path: "///earth//////cat.jpg"}, "../"},
	}

	for _, fix := range fixtures {
		relativeURL := urlRelativeToRoot(&fix.SrcURL)
		assert.Equal(t, fix.Expected, relativeURL)
	}
}
