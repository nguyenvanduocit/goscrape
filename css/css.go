// Package css provides a CSS parser that can process CSS data and call a processor for every found URL.
package css

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/cornelk/gotokit/log"
	"github.com/gorilla/css/scanner"
)

var cssURLRe = regexp.MustCompile(`^url\(['"]?(.*?)['"]?\)$`)

// Token represents a token and the corresponding string.
type Token = scanner.Token

// TokenKind classifies a URL reference for callers so they can apply the
// right replacement strategy. KindImportString tokens carry the FULL source
// pattern (e.g. `@import "x.css"`) in Token.Value so callers can use it as
// a unique replacement key — replacing the bare `"x.css"` would collide
// with unrelated string literals like `content: "x.css"`.
type TokenKind int

const (
	// KindURL is a url(...) reference. Token.Value is the literal url(...)
	// substring suitable for direct replacement.
	KindURL TokenKind = iota
	// KindImportString is the bare-string form of @import (e.g.
	// `@import "x.css"` or `@import 'x.css'`). Token.Value is the FULL
	// source substring (`@import "x.css"`) including the at-keyword and
	// any whitespace/comment separator, so callers can rewrite the entire
	// statement to `@import url('resolved.css')` safely.
	KindImportString
)

// Kind reports the URL-reference flavor of a token received by a urlProcessor.
func Kind(t *Token) TokenKind {
	if t.Type == scanner.TokenString {
		return KindImportString
	}
	return KindURL
}

type urlProcessor func(token *Token, data string, url *url.URL)

// Process scans CSS data and invokes the processor for every URL it finds.
// It handles three URL-bearing forms:
//   - url("…") / url('…') / url(…)        — anywhere (KindURL)
//   - @import url("…")                    — @import + url() form (KindURL)
//   - @import "…" / @import '…'           — @import + bare-string form
//     (KindImportString — Token.Value carries the full source substring)
//
// The bare-string form requires a small state machine because the scanner
// reports it as a generic STRING token; we only treat a STRING as a URL
// when it directly follows an @import at-keyword (with optional whitespace
// or CSS comments).
func Process(logger *log.Logger, baseURL *url.URL, data string, processor urlProcessor) {
	css := scanner.New(data)
	pendingImport := false
	var atKeywordRaw string
	var separator strings.Builder

	resetImport := func() {
		pendingImport = false
		atKeywordRaw = ""
		separator.Reset()
	}

	for {
		token := css.Next()
		if token.Type == scanner.TokenEOF || token.Type == scanner.TokenError {
			break
		}

		// Whitespace + comments don't clear @import state and contribute to
		// the source pattern we reconstruct for bare-string imports.
		if token.Type == scanner.TokenS || token.Type == scanner.TokenComment {
			if pendingImport {
				separator.WriteString(token.Value)
			}
			continue
		}

		switch token.Type {
		case scanner.TokenAtKeyword:
			if strings.EqualFold(token.Value, "@import") {
				pendingImport = true
				atKeywordRaw = token.Value
				separator.Reset()
			} else {
				resetImport()
			}
			continue
		case scanner.TokenURI:
			handleURIToken(logger, baseURL, token, processor)
			resetImport()
			continue
		case scanner.TokenString:
			if pendingImport {
				handleStringImport(logger, baseURL, token, atKeywordRaw, separator.String(), processor)
				resetImport()
				continue
			}
		}

		// Any other non-whitespace token (`;`, `{`, `}`, idents, media queries,
		// etc.) ends the @import context.
		resetImport()
	}
}

// handleURIToken decodes a "url(...)" token and dispatches to the processor.
// Used both for plain url(...) anywhere in CSS and for @import url(...).
func handleURIToken(logger *log.Logger, baseURL *url.URL, token *Token, processor urlProcessor) {
	match := cssURLRe.FindStringSubmatch(token.Value)
	if match == nil {
		return
	}
	dispatchURL(logger, baseURL, token, match[1], processor)
}

// handleStringImport decodes an `@import "…"` bare-string token. The scanner
// includes the surrounding quote in token.Value, so we trim a single matching
// pair of quotes to extract the raw URL. To let callers safely rewrite the
// entire statement without colliding with unrelated `"x.css"` literals
// elsewhere in the CSS, we synthesize a new Token whose Value is the FULL
// source substring (at-keyword + separator + quoted string).
func handleStringImport(logger *log.Logger, baseURL *url.URL, token *Token, atKeywordRaw, separator string, processor urlProcessor) {
	quoted := token.Value
	src := quoted
	if len(src) >= 2 {
		first, last := src[0], src[len(src)-1]
		if (first == '"' || first == '\'') && first == last {
			src = src[1 : len(src)-1]
		}
	}
	synthesized := &Token{
		Type:   scanner.TokenString,
		Value:  atKeywordRaw + separator + quoted,
		Line:   token.Line,
		Column: token.Column,
	}
	dispatchURL(logger, baseURL, synthesized, src, processor)
}

// dispatchURL is the shared tail used by both URI and STRING handlers.
// It filters out data: URIs and parse failures, then invokes the processor.
func dispatchURL(logger *log.Logger, baseURL *url.URL, token *Token, src string, processor urlProcessor) {
	if strings.HasPrefix(strings.ToLower(src), "data:") {
		return
	}
	u, err := baseURL.Parse(src)
	if err != nil {
		logger.Error("Parsing URL failed",
			log.String("url", src),
			log.Err(err))
		return
	}
	processor(token, src, u)
}
