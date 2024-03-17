package htmlindex

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// Index provides an index for all HTML tags of relevance for scraping.
type Index struct {
	// key is HTML tag, value is a map of all its urls and the HTML nodes for it
	data map[string]map[string][]*html.Node
}

// New returns a new index.
func New() *Index {
	return &Index{
		data: make(map[string]map[string][]*html.Node),
	}
}

// Index the given HTML document.
func (h *Index) Index(baseURL *url.URL, node *html.Node) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != html.ElementNode {
			continue
		}

		var references []string

		switch child.Data {
		case "a", "link":
			references = nodeURL(baseURL, "href", child)

		case "img":
			references = nodeURL(baseURL, "src", child)
			references = append(references, nodeURL(baseURL, "srcset", child)...)

		case "script":
			references = nodeURL(baseURL, "src", child)
		default:
			if node.FirstChild != nil {
				h.Index(baseURL, child)
			}
			continue
		}

		if len(references) == 0 {
			continue
		}

		m, ok := h.data[child.Data]
		if !ok {
			m = map[string][]*html.Node{}
			h.data[child.Data] = m
		}
		for _, reference := range references {
			m[reference] = append(m[reference], child)
		}

		// Recurse into the child node
		h.Index(baseURL, child)
	}

}

// URLs returns all URLs of the references found for a specific tag.
func (h *Index) URLs(tag string) ([]*url.URL, error) {
	m, ok := h.data[tag]
	if !ok {
		return nil, nil
	}

	data := make([]string, 0, len(m))
	for key := range m {
		data = append(data, key)
	}
	sort.Strings(data)

	urls := make([]*url.URL, 0, len(m))
	for _, fullURL := range data {
		u, err := url.Parse(fullURL)
		if err != nil {
			return nil, fmt.Errorf("parsing URL '%s': %w", fullURL, err)
		}
		urls = append(urls, u)
	}

	return urls, nil
}

// Nodes returns a map of all URLs and their HTML nodes.
func (h *Index) Nodes(tag string) map[string][]*html.Node {
	m, ok := h.data[tag]
	if ok {
		return m
	}
	return map[string][]*html.Node{}
}

func nodeURL(baseURL *url.URL, attributeName string, node *html.Node) []string {
	var urls []string
	for _, attr := range node.Attr {
		if attr.Key != attributeName {
			continue
		}

		references := strings.Split(attr.Val, ",")
		for _, reference := range references {
			reference = strings.TrimSpace(strings.Split(reference, " ")[0])
			ur, err := url.Parse(reference)
			if err != nil {
				continue
			}

			resolvedURL := baseURL.ResolveReference(ur)
			urls = append(urls, resolvedURL.String())
		}
	}

	return urls
}
