# HTML indexing and CSS processing

**Audience:** Engineers modifying how URLs are discovered from HTML or CSS content.

**Primary tasks:**
1. Add support for a new HTML tag or attribute that contains URLs.
2. Understand how CSS `url()` references are extracted.
3. Trace why a specific URL was or was not discovered during scraping.

**Decision horizon:** Feature development, correctness bugs.

**Out of scope:** How discovered URLs are downloaded or rewritten (see [scraper-core.md](scraper-core.md)).

---

## HTML indexing (`htmlindex/`)

The `htmlindex` package parses an HTML document and produces a tag-keyed index of all URLs. The scraper uses this index to know what to download and what HTML attributes to rewrite.

### Index struct

`Index` (`htmlindex/htmlindex.go:17`) stores discovered URLs in a two-level map:

```
data map[string]              // outer key: HTML tag name (e.g. "img", "link")
         map[string]          // inner key: resolved absolute URL string
             []*html.Node     // all nodes that reference this URL
```

The `[]*html.Node` slice allows the scraper to rewrite the attribute value in-place — the index holds direct pointers into the parsed HTML tree.

`New()` (`htmlindex/htmlindex.go:25`) creates a fresh index with an empty map. One `Index` per page.

### Indexing an HTML document

`Index.Index()` (`htmlindex/htmlindex.go:33`) walks the HTML tree recursively, calling `indexElementNode()` for each `html.ElementNode`.

`indexElementNode()` (`htmlindex/htmlindex.go:43`):
1. Looks up the element tag in `Nodes` (the tag → attribute map).
2. Calls `nodeAttributeURLs()` to extract and resolve URLs from relevant attributes.
3. Stores node pointers in `data[tag][resolvedURL]`.
4. Calls `indexInlineStyle()` to handle `style="..."` attributes on any element.
5. Recurses into children unless the node has `noChildParsing: true` (only `<style>` tags set this, because their content is text, not child elements).

### Tag and attribute catalog (`attributes.go`)

The `Nodes` map (`htmlindex/attributes.go:56`) defines which tags and attributes contain URLs:

| Tag constant | HTML element | Attributes scanned |
|---|---|---|
| `ATag` (`"a"`) | Hyperlinks | `href` |
| `BodyTag` (`"body"`) | Page body | `background` |
| `ImgTag` (`"img"`) | Images | `src`, `data-src`, `srcset`, `data-srcset` |
| `LinkTag` (`"link"`) | Stylesheets, preloads | `href` |
| `ScriptTag` (`"script"`) | JavaScript files | `src` |
| `StyleTag` (`"style"`) | Embedded CSS | content via `styleParser` |

`InlineStyleTag` (`"__inline_style__"`) is a virtual tag — not an HTML element — used to store URLs found in `style="..."` attributes on any element (`htmlindex/attributes.go:52`).

To add a new HTML tag: add an entry to `Nodes` in `htmlindex/attributes.go`. To add a new attribute on an existing tag: append to its `Attributes` slice.

### Custom attribute parsers

Two tags use custom `nodeAttributeParser` functions instead of plain attribute value extraction:

**`srcSetValueSplitter`** (`htmlindex/htmlindex.go:197`) — handles `srcset` and `data-srcset`. Splits the comma-separated list of `url width` pairs and extracts the URL part from each entry.

**`styleParser`** (`htmlindex/htmlindex.go:215`) — handles `<style>` tag content. Reads `node.FirstChild.Data` (the raw CSS text) and delegates to `css.Process()` to extract `url()` references.

**`inlineStyleParser`** (`htmlindex/htmlindex.go:232`) — handles `style="..."` attributes. Passes the attribute value directly to `css.Process()`.

### `SrcSetAttributes` set

`SrcSetAttributes` (`htmlindex/attributes.go:80`) is a set of attribute names that use srcset syntax. The HTML rewriting layer (`scraper/html.go:111`) checks this set to decide whether to call `resolveSrcSetURLs` instead of `resolveURL`.

### URLs() and Nodes() accessors

`URLs(tag)` (`htmlindex/htmlindex.go:106`) returns sorted, deduplicated `[]*url.URL` for a tag. Sorting ensures deterministic download order — important for reproducible output and testing.

`Nodes(tag)` (`htmlindex/htmlindex.go:131`) returns the raw `map[string][]*html.Node` for direct node mutation during URL rewriting.

---

## CSS processing (`css/`)

The `css` package tokenizes CSS content and calls a user-supplied callback for each `url()` reference found.

### Process function

`css.Process()` (`css/css.go:21`) is the only exported function. Signature:

```go
func Process(logger *log.Logger, url *url.URL, data string, processor urlProcessor)
```

- `url`: the base URL of the CSS file (used by callers to resolve relative URLs).
- `data`: raw CSS text.
- `processor`: callback called for each `url()` token found.

The function uses `gorilla/css/scanner` to tokenize the CSS stream. It processes only `TokenURI` tokens — everything else is ignored. For each URI token:

1. Applies `cssURLRe` (`css/css.go:13`) to strip `url(...)` wrapper and optional quotes.
2. Skips `data:` URIs.
3. Parses the extracted string as a URL.
4. Calls `processor(token, rawURLString, parsedURL)`.

### Why a token scanner instead of regex

CSS `url()` values can contain quoted strings (`url("foo.png")`), unquoted values (`url(foo.png)`), and single-quoted variants (`url('foo.png')`). The gorilla CSS scanner handles all these cases correctly, including edge cases like whitespace inside `url(...)`. A raw regex would require covering all three quoting styles and could miss CSS-specific escape sequences.

### Callers of css.Process()

Three callers exist, each with a different purpose:

| Caller | File | Purpose |
|---|---|---|
| `styleParser` | `htmlindex/htmlindex.go:215` | Discover URLs during HTML indexing |
| `inlineStyleParser` | `htmlindex/htmlindex.go:232` | Discover URLs from inline style attributes |
| `cssProcessor` | `scraper/download.go:258` | Rewrite URLs in downloaded CSS files |
| `fixStyleTagURL` | `scraper/html.go:144` | Rewrite URLs in `<style>` tag content |
| `fixInlineStyleURL` | `scraper/html.go:183` | Rewrite URLs in inline style attributes |

The discovery callers (htmlindex) collect URLs. The rewriting callers (scraper) both collect and rewrite.

---

## Data flow: URL discovery to scraper

```
htmlindex.Index.Index(baseURL, doc)
  → indexElementNode()
      → nodeAttributeURLs()        # plain attribute → resolve → store in data map
      → srcSetValueSplitter()      # srcset → split → resolve → store
      → styleParser()              # <style> content → css.Process() → store
      → indexInlineStyle()         # style="..." → css.Process() → store
  → (recursion to children)

scraper.processURL()
  → index.URLs(htmlindex.ImgTag)   # returns sorted []*url.URL
  → index.URLs(htmlindex.ATag)     # hyperlinks for BFS queue
  → index.Nodes(tag)               # for in-place HTML attribute rewriting
```

---

## Failure modes

| Failure | What happens |
|---|---|
| Malformed URL in attribute | `url.Parse` error → URL silently skipped during indexing |
| CSS tokenizer error (`TokenError`) | Scan stops at that position; URLs after the error are not found |
| `<style>` tag with no text content (`FirstChild == nil`) | `styleParser` returns nil — no URLs extracted (correct behavior) |
| `style=""` empty attribute | `inlineStyleParser` returns nil immediately (`htmlindex/htmlindex.go:233`) |
| Circular inline style references (e.g. `style="background: url(self.css)"`) | No cycle detection — the URL is discovered and enqueued normally |

---

## Blast radius: safe change guide

| Change | Risk | Notes |
|---|---|---|
| Add entry to `Nodes` map | Low | Extend `htmlindex_test.go` with a test for the new tag |
| Add attribute to existing `Nodes` entry | Low | Same as above |
| Change `styleParser` or `inlineStyleParser` | Medium | Also affects scraper's rewriting path since both use `css.Process()` |
| Change `css.Process()` | High | All CSS URL discovery and rewriting depends on it |
| Change `srcSetValueSplitter` | Low–Medium | Only affects responsive image handling |
| Change the `data` map structure in `Index` | High | Callers in `scraper/download.go` and `scraper/html.go` depend on both `URLs()` and `Nodes()` |

---

## Unknowns

| Unknown | Verification step |
|---|---|
| Behavior when a `<link>` tag uses `rel="preload"` with `as="font"` — does the font URL get downloaded? | Trace `nodeAttributeURLs` for a page with font preload links |
| Whether `data-src` / `data-srcset` lazy-loaded images are triggered correctly by JavaScript | Manual test with a lazy-load image library |

---

<!-- ORACLE-META
Written by codebase-oracle | 2026-03-13
Data: direct source reading of htmlindex/ (3 files) + css/ (1 file)
Audience: feature engineers | Confidence: 95%
Unknowns: 2 items pending verification
-->
