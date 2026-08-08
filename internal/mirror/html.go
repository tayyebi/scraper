package mirror

import (
	"strings"

	"github.com/tayyebi/scraper/internal/core"
)

// voidElements have no closing tag and no children. Emitting </br> produces a
// document that parses differently from the one that was captured, which
// defeats the purpose of serializing at all.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

// rawTextElements hold text that must not be entity-escaped: escaping the `<`
// in `if (a < b)` inside a <script> would change the program.
var rawTextElements = map[string]bool{
	"script": true, "style": true,
}

// RenderHTML re-serializes a mirrored document back into HTML.
//
// The output is a faithful rendering of the mirror, which is not the same thing
// as the bytes the server sent: it reflects the DOM after scripts ran, which is
// usually what a scraper actually wanted and never what a plain HTTP fetch
// returns.
func RenderHTML(doc core.Document) string {
	var b strings.Builder
	writeNode(&b, doc.Root)
	return b.String()
}

func writeNode(b *strings.Builder, n *core.Node) {
	if n == nil {
		return
	}
	switch n.Type {
	case core.NodeDocument, core.NodeFragment:
		for _, kid := range n.Kids {
			writeNode(b, kid)
		}

	case core.NodeDoctype:
		b.WriteString("<!DOCTYPE ")
		name := n.Name
		if name == "" {
			name = "html"
		}
		b.WriteString(name)
		b.WriteString(">\n")

	case core.NodeText:
		// Raw-text content is handled by writeElement, which knows its own tag.
		// A text node reached through here is always escaped.
		b.WriteString(escapeText(n.Value))

	case core.NodeComment:
		b.WriteString("<!--")
		b.WriteString(n.Value)
		b.WriteString("-->")

	case core.NodeElement:
		writeElement(b, n)

	default:
		for _, kid := range n.Kids {
			writeNode(b, kid)
		}
	}
}

func writeElement(b *strings.Builder, n *core.Node) {
	tag := strings.ToLower(n.Name)
	if tag == "" {
		tag = "div"
	}

	b.WriteString("<")
	b.WriteString(tag)
	for _, a := range n.Attrs {
		b.WriteString(" ")
		b.WriteString(a.Name)
		b.WriteString(`="`)
		b.WriteString(escapeAttr(a.Value))
		b.WriteString(`"`)
	}
	// A frame owner is marked rather than inlined: a child frame is a separate
	// document with its own URL and origin, and splicing it into the parent
	// would produce HTML that never existed in the browser.
	if n.Frame != "" {
		b.WriteString(` data-hub-frame="`)
		b.WriteString(escapeAttr(n.Frame))
		b.WriteString(`"`)
	}
	b.WriteString(">")

	if voidElements[tag] {
		return
	}

	// An open shadow root serializes as a declarative shadow DOM template,
	// which is the standard way to express one in static HTML. Closed shadow
	// roots are unreachable from script and therefore absent from the mirror
	// entirely -- documented, not worked around.
	if n.Shadow != nil {
		b.WriteString(`<template shadowrootmode="open">`)
		for _, kid := range n.Shadow.Kids {
			writeNode(b, kid)
		}
		b.WriteString("</template>")
	}

	raw := rawTextElements[tag]
	for _, kid := range n.Kids {
		if raw && kid.Type == core.NodeText {
			b.WriteString(kid.Value)
			continue
		}
		writeNode(b, kid)
	}

	b.WriteString("</")
	b.WriteString(tag)
	b.WriteString(">")
}

// escapeText escapes the three characters that change parsing in text content.
// Quotes are left alone: they are not special outside an attribute value, and
// escaping them makes captured text needlessly unreadable.
func escapeText(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeAttr escapes what would otherwise terminate or alter a double-quoted
// attribute value.
func escapeAttr(s string) string {
	if !strings.ContainsAny(s, `&<>"`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Text extracts a document's visible text, skipping script and style content.
// The Control API offers it because "give me the text of this page" is the most
// common thing a caller does with a DOM snapshot.
func Text(doc core.Document) string {
	var b strings.Builder
	collectText(&b, doc.Root)
	return strings.TrimSpace(b.String())
}

func collectText(b *strings.Builder, n *core.Node) {
	if n == nil {
		return
	}
	if n.Type == core.NodeElement && rawTextElements[strings.ToLower(n.Name)] {
		return
	}
	if n.Type == core.NodeText {
		b.WriteString(n.Value)
		return
	}
	if n.Shadow != nil {
		collectText(b, n.Shadow)
	}
	for _, kid := range n.Kids {
		collectText(b, kid)
		if kid.Type == core.NodeElement && isBlock(kid.Name) {
			b.WriteString("\n")
		}
	}
}

var blockElements = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"div": true, "dl": true, "fieldset": true, "figure": true, "footer": true,
	"form": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "header": true, "hr": true, "li": true, "main": true, "nav": true,
	"ol": true, "p": true, "pre": true, "section": true, "table": true,
	"tr": true, "ul": true,
}

func isBlock(tag string) bool { return blockElements[strings.ToLower(tag)] }
