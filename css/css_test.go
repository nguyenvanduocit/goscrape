package css

import (
	"net/url"
	"testing"

	"github.com/cornelk/gotokit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectProcessedURLs runs Process and returns the list of src strings the
// processor sees. Used to assert that @import / url() forms are detected.
func collectProcessedURLs(t *testing.T, css string) []string {
	t.Helper()
	base, err := url.Parse("https://example.org/base/")
	require.NoError(t, err)

	var got []string
	Process(log.NewNop(), base, css, func(_ *Token, src string, _ *url.URL) {
		got = append(got, src)
	})
	return got
}

func TestProcess_AtImportURLForm(t *testing.T) {
	got := collectProcessedURLs(t, `@import url("theme.css");`)
	assert.Equal(t, []string{"theme.css"}, got)
}

func TestProcess_AtImportStringDoubleQuote(t *testing.T) {
	got := collectProcessedURLs(t, `@import "theme.css";`)
	assert.Equal(t, []string{"theme.css"}, got, "bare-string @import must be detected")
}

func TestProcess_AtImportStringSingleQuote(t *testing.T) {
	got := collectProcessedURLs(t, `@import 'theme.css';`)
	assert.Equal(t, []string{"theme.css"}, got)
}

func TestProcess_AtImportStringWithMediaQuery(t *testing.T) {
	got := collectProcessedURLs(t, `@import "theme.css" screen and (max-width: 600px);`)
	assert.Equal(t, []string{"theme.css"}, got)
}

func TestProcess_AtImportURLWithMediaQuery(t *testing.T) {
	got := collectProcessedURLs(t, `@import url("theme.css") screen;`)
	assert.Equal(t, []string{"theme.css"}, got)
}

func TestProcess_PlainURLAnywhere(t *testing.T) {
	got := collectProcessedURLs(t, `body { background: url(/bg.png); }`)
	assert.Equal(t, []string{"/bg.png"}, got)
}

func TestProcess_FontFaceURLNotMistakenAsImport(t *testing.T) {
	got := collectProcessedURLs(t,
		`@font-face { font-family: "Sans"; src: url("/font.woff2"); }`)
	assert.Equal(t, []string{"/font.woff2"}, got,
		"@font-face url(...) must still be processed; the @font-face string must NOT be")
}

func TestProcess_StandaloneStringNotEnqueued(t *testing.T) {
	// A string in a `content: "..."` rule must NOT be treated as a URL.
	got := collectProcessedURLs(t, `body:before { content: "theme.css"; }`)
	assert.Empty(t, got)
}

func TestProcess_AtImportDataURLSkipped(t *testing.T) {
	got := collectProcessedURLs(t, `@import "data:text/css,body{}";`)
	assert.Empty(t, got, "data: URIs must be skipped")
}

func TestProcess_MultipleImports(t *testing.T) {
	got := collectProcessedURLs(t, `
		@import "a.css";
		@import url("b.css");
		@import 'c.css' screen;
		body { background: url(/d.png); }
	`)
	assert.Equal(t, []string{"a.css", "b.css", "c.css", "/d.png"}, got)
}

func TestProcess_AtImportContextResetOnSemicolon(t *testing.T) {
	// After "@import "a.css";", a following bare "b.css" string in content
	// must NOT be treated as an import target.
	got := collectProcessedURLs(t, `@import "a.css"; body:after { content: "b.css"; }`)
	assert.Equal(t, []string{"a.css"}, got)
}

// TestProcess_ImportStringTokenValueIsUniqueAnchor — guards Codex's BLOCK:
// the synthesized token for an @import string carries the full "@import …"
// pattern as Token.Value so callers using it as a NewReplacer key can't
// accidentally rewrite an unrelated `"x.css"` string literal elsewhere.
func TestProcess_ImportStringTokenValueIsUniqueAnchor(t *testing.T) {
	base, err := url.Parse("https://example.org/")
	require.NoError(t, err)

	cssData := `@import "x.css"; body:after { content: "x.css"; }`

	var importTokenValue string
	Process(log.NewNop(), base, cssData, func(tok *Token, _ string, _ *url.URL) {
		if Kind(tok) == KindImportString {
			importTokenValue = tok.Value
		}
	})
	require.NotEmpty(t, importTokenValue,
		"@import string must produce a KindImportString token")
	assert.True(t,
		len(importTokenValue) > len(`"x.css"`) && importTokenValue != `"x.css"`,
		"Token.Value for @import string must include the @import prefix, got: %q", importTokenValue)
	assert.Contains(t, importTokenValue, "@import")
}

// TestProcess_AtImportPreservesComment — CSS comments between @import and
// the bare string are valid whitespace; the synthesized pattern must include
// them so a later string-replace finds and rewrites the full source span.
func TestProcess_AtImportPreservesComment(t *testing.T) {
	base, err := url.Parse("https://example.org/")
	require.NoError(t, err)

	cssData := `@import/*c*/ "x.css";`
	var importTokenValue string
	Process(log.NewNop(), base, cssData, func(tok *Token, _ string, _ *url.URL) {
		if Kind(tok) == KindImportString {
			importTokenValue = tok.Value
		}
	})
	assert.Contains(t, importTokenValue, "/*c*/",
		"comment between @import and string must be part of the replacement anchor")
}
