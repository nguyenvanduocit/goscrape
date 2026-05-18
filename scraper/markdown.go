package scraper

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"golang.org/x/net/html"
)

// Package-level compiled regexes for markdown cleanup.
var (
	emptyImagePattern         = regexp.MustCompile(`!\[\s*\]\(\s*\)`)
	autolinkConvertPattern    = regexp.MustCompile(`\[\s*\]\(([^)\s][^)]*)\)`)
	textOnlyLinkPattern       = regexp.MustCompile(`\[([^\]]+)\]\(\s*\)`)
	emptyLinkPattern          = regexp.MustCompile(`\[\s*\]\(\s*\)`)
	emptyHeadingPattern       = regexp.MustCompile(`^\s*#{1,6}\s*$`)
	trailingWhitespacePattern = regexp.MustCompile(`[ \t]+$`)
	multiBlankPattern         = regexp.MustCompile(`\n{3,}`)
)

// convertToMarkdown converts an HTML document AST to Markdown bytes with
// YAML frontmatter. The pageURL is used to populate the frontmatter url field.
func (s *Scraper) convertToMarkdown(doc *html.Node, pageURL *url.URL) ([]byte, error) {
	// Extract title before conversion — ConvertNode may alter the tree.
	title := extractTitle(doc)

	md, err := htmltomarkdown.ConvertNode(doc)
	if err != nil {
		return nil, fmt.Errorf("converting HTML to Markdown: %w", err)
	}

	md = cleanupMarkdown(md)

	scrapedAt := time.Now().UTC().Format(time.RFC3339)
	fm := buildFrontmatter(title, pageURL.String(), scrapedAt)

	return append(fm, md...), nil
}

// extractTitle walks the HTML document tree and returns the text content of
// the first <title> element. Internal whitespace is collapsed to single spaces.
// Returns "" if no title is found or it is whitespace-only.
func extractTitle(doc *html.Node) string {
	var title string
	var found bool

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found {
			return
		}
		if n.Type == html.ElementNode && n.Data == "title" {
			title = collectText(n)
			found = true
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
			if found {
				return
			}
		}
	}
	walk(doc)

	// Flatten internal whitespace (tabs, newlines, multi-spaces) to single space.
	title = strings.Join(strings.Fields(title), " ")
	return title
}

// collectText concatenates all text node content under a given node.
func collectText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

// yamlEscapeString escapes backslashes and double quotes for YAML double-quoted strings.
func yamlEscapeString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// buildFrontmatter produces YAML frontmatter bytes. The title field is omitted
// when empty. A trailing blank line separates frontmatter from the body.
func buildFrontmatter(title, urlStr, scrapedAt string) []byte {
	var sb strings.Builder
	sb.WriteString("---\n")
	if title != "" {
		sb.WriteString(`title: "`)
		sb.WriteString(yamlEscapeString(title))
		sb.WriteString("\"\n")
	}
	sb.WriteString(`url: "`)
	sb.WriteString(yamlEscapeString(urlStr))
	sb.WriteString("\"\n")
	sb.WriteString(`scraped_at: "`)
	sb.WriteString(yamlEscapeString(scrapedAt))
	sb.WriteString("\"\n")
	sb.WriteString("---\n\n")
	return []byte(sb.String())
}

// replaceAutolinks converts [](url) to <url> while preserving ![](url) images.
// Go regexp does not support negative lookbehind, so we walk match positions manually.
func replaceAutolinks(line string) string {
	indexes := autolinkConvertPattern.FindAllStringSubmatchIndex(line, -1)
	if len(indexes) == 0 {
		return line
	}

	var sb strings.Builder
	prev := 0
	for _, loc := range indexes {
		matchStart := loc[0]
		// If preceded by '!', this is an image — preserve it.
		if matchStart > 0 && line[matchStart-1] == '!' {
			sb.WriteString(line[prev:loc[1]])
			prev = loc[1]
			continue
		}
		sb.WriteString(line[prev:matchStart])
		// loc[2]:loc[3] is the capture group (the URL).
		sb.WriteString("<")
		sb.WriteString(line[loc[2]:loc[3]])
		sb.WriteString(">")
		prev = loc[1]
	}
	sb.WriteString(line[prev:])
	return sb.String()
}

// cleanupMarkdown applies deterministic cleanup rules to converter output.
// Fenced code blocks (``` or ~~~) are preserved verbatim.
func cleanupMarkdown(md []byte) []byte {
	lines := strings.Split(string(md), "\n")
	out := make([]string, 0, len(lines))

	inFence := false
	fenceMarker := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if inFence {
			out = append(out, line)
			if strings.HasPrefix(trimmed, fenceMarker) {
				inFence = false
				fenceMarker = ""
			}
			continue
		}

		// Check for fence opening.
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = true
			if strings.HasPrefix(trimmed, "```") {
				fenceMarker = "```"
			} else {
				fenceMarker = "~~~"
			}
			out = append(out, line)
			continue
		}

		// Apply cleanup rules in order (images before links).
		line = emptyImagePattern.ReplaceAllString(line, "")
		line = replaceAutolinks(line)
		line = textOnlyLinkPattern.ReplaceAllString(line, "$1")
		line = emptyLinkPattern.ReplaceAllString(line, "")

		// Strip trailing whitespace.
		line = trailingWhitespacePattern.ReplaceAllString(line, "")

		// Drop empty headings.
		if emptyHeadingPattern.MatchString(line) {
			continue
		}

		out = append(out, line)
	}

	joined := strings.Join(out, "\n")

	// Collapse 3+ consecutive blank lines to 2.
	joined = multiBlankPattern.ReplaceAllString(joined, "\n\n")

	// Clean leading/trailing blank lines, ensure terminating newline.
	joined = strings.TrimSpace(joined) + "\n"

	return []byte(joined)
}
