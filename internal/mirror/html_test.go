package mirror

import (
	"strings"
	"testing"

	"github.com/tayyebi/scraper/internal/core"
)

func el(id int64, name string, kids ...*core.Node) *core.Node {
	return &core.Node{ID: id, Type: core.NodeElement, Name: name, Kids: kids}
}

func text(id int64, s string) *core.Node {
	return &core.Node{ID: id, Type: core.NodeText, Value: s}
}

func wrap(root *core.Node) core.Document {
	return core.Document{Root: &core.Node{ID: 1, Type: core.NodeDocument, Kids: []*core.Node{root}}}
}

func TestRenderBasicTree(t *testing.T) {
	d := wrap(el(2, "div", text(3, "hello")))
	if got, want := RenderHTML(d), "<div>hello</div>"; got != want {
		t.Errorf("RenderHTML = %q, want %q", got, want)
	}
}

func TestRenderAttributes(t *testing.T) {
	n := el(2, "a", text(3, "link"))
	n.Attrs = []core.Attr{
		{Name: "href", Value: "https://example.test/?a=1&b=2"},
		{Name: "title", Value: `he said "hi"`},
	}
	got := RenderHTML(wrap(n))
	if !strings.Contains(got, `href="https://example.test/?a=1&amp;b=2"`) {
		t.Errorf("ampersand not escaped in attribute: %s", got)
	}
	if !strings.Contains(got, `title="he said &quot;hi&quot;"`) {
		t.Errorf("quote not escaped in attribute: %s", got)
	}
}

// Emitting </br> produces a document that parses differently from the one that
// was captured, which defeats the point of serializing.
func TestVoidElementsHaveNoClosingTag(t *testing.T) {
	for _, tag := range []string{"br", "img", "input", "meta", "hr", "link"} {
		got := RenderHTML(wrap(el(2, tag)))
		if strings.Contains(got, "</"+tag+">") {
			t.Errorf("%s serialized with a closing tag: %s", tag, got)
		}
	}
	// And a non-void element still gets one.
	if got := RenderHTML(wrap(el(2, "div"))); !strings.Contains(got, "</div>") {
		t.Errorf("div lost its closing tag: %s", got)
	}
}

// Escaping the `<` in `if (a < b)` would change the program.
func TestScriptAndStyleContentIsNotEscaped(t *testing.T) {
	script := RenderHTML(wrap(el(2, "script", text(3, "if (a < b && c > d) { x(); }"))))
	if !strings.Contains(script, "if (a < b && c > d)") {
		t.Errorf("script body was entity-escaped: %s", script)
	}

	style := RenderHTML(wrap(el(2, "style", text(3, "a > b { color: red }"))))
	if !strings.Contains(style, "a > b") {
		t.Errorf("style body was entity-escaped: %s", style)
	}

	// Ordinary text still is.
	p := RenderHTML(wrap(el(2, "p", text(3, "a < b & c"))))
	if !strings.Contains(p, "a &lt; b &amp; c") {
		t.Errorf("text content was not escaped: %s", p)
	}
}

func TestRenderDoctypeAndComments(t *testing.T) {
	d := core.Document{Root: &core.Node{
		ID: 1, Type: core.NodeDocument,
		Kids: []*core.Node{
			{ID: 2, Type: core.NodeDoctype, Name: "html"},
			{ID: 3, Type: core.NodeComment, Value: " build 42 "},
			el(4, "html"),
		},
	}}
	got := RenderHTML(d)
	if !strings.HasPrefix(got, "<!DOCTYPE html>") {
		t.Errorf("missing doctype: %s", got)
	}
	if !strings.Contains(got, "<!-- build 42 -->") {
		t.Errorf("missing comment: %s", got)
	}
}

// An open shadow root round-trips as a declarative shadow DOM template, which
// is the standard way to express one in static HTML.
func TestOpenShadowRootSerializesAsTemplate(t *testing.T) {
	host := el(2, "my-widget")
	host.Shadow = &core.Node{
		ID: 3, Type: core.NodeFragment,
		Kids: []*core.Node{el(4, "span", text(5, "inside"))},
	}
	got := RenderHTML(wrap(host))
	if !strings.Contains(got, `<template shadowrootmode="open">`) {
		t.Errorf("shadow root not serialized as a declarative template: %s", got)
	}
	if !strings.Contains(got, "<span>inside</span>") {
		t.Errorf("shadow content missing: %s", got)
	}
	if !strings.Contains(got, "</template>") {
		t.Errorf("template not closed: %s", got)
	}
}

// A child frame is a separate document with its own URL and origin. Splicing it
// into the parent would produce HTML that never existed in the browser, so the
// owner is marked instead.
func TestFrameOwnerIsMarkedNotInlined(t *testing.T) {
	iframe := el(2, "iframe")
	iframe.Frame = "frame-7"
	iframe.Attrs = []core.Attr{{Name: "src", Value: "https://embed.example.test/"}}

	got := RenderHTML(wrap(iframe))
	if !strings.Contains(got, `data-hub-frame="frame-7"`) {
		t.Errorf("frame owner not marked: %s", got)
	}
}

func TestRenderEmptyDocument(t *testing.T) {
	if got := RenderHTML(core.Document{}); got != "" {
		t.Errorf("RenderHTML(empty) = %q, want empty", got)
	}
}

func TestTextExtraction(t *testing.T) {
	body := el(2, "body",
		el(3, "h1", text(4, "Title")),
		el(5, "script", text(6, "console.log('not text')")),
		el(7, "style", text(8, "body{color:red}")),
		el(9, "p", text(10, "First paragraph.")),
		el(11, "p", text(12, "Second paragraph.")),
	)
	got := Text(wrap(body))

	for _, want := range []string{"Title", "First paragraph.", "Second paragraph."} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() lost %q: %q", want, got)
		}
	}
	for _, unwanted := range []string{"console.log", "color:red"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("Text() included script or style content %q: %q", unwanted, got)
		}
	}
	if !strings.Contains(got, "First paragraph.\n") {
		t.Errorf("block elements did not produce line breaks: %q", got)
	}
}

// The round trip that matters: snapshot, mutate, re-serialize, and the HTML
// reflects the mutation rather than the original page.
func TestMutatedMirrorSerializesToUpdatedHTML(t *testing.T) {
	m := newMirror("s_1")
	m.ApplySnapshot(MainFrame, doc(), 1)

	if err := m.ApplyMutations(2, []Op{
		{Kind: OpText, ID: 6, Value: str("mutated")},
		{Kind: OpAttr, ID: 4, Name: "data-state", Value: str("ready")},
	}); err != nil {
		t.Fatalf("ApplyMutations: %v", err)
	}

	d, ok := m.Document()
	if !ok {
		t.Fatal("no document")
	}
	got := RenderHTML(d)
	if !strings.Contains(got, "mutated") {
		t.Errorf("HTML does not reflect the text mutation: %s", got)
	}
	if !strings.Contains(got, `data-state="ready"`) {
		t.Errorf("HTML does not reflect the attribute mutation: %s", got)
	}
	if strings.Contains(got, "hello") {
		t.Errorf("HTML still shows the pre-mutation text: %s", got)
	}
}
