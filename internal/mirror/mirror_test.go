package mirror

import (
	"errors"
	"strings"
	"testing"

	"github.com/tayyebi/scraper/internal/core"
)

func str(s string) *string { return &s }

// doc builds: <html><body><div id="root"><p>hello</p></div></body></html>
func doc() core.Document {
	return core.Document{
		URL:   "https://example.test/",
		Title: "Example",
		Root: &core.Node{
			ID: 1, Type: core.NodeDocument,
			Kids: []*core.Node{
				{
					ID: 2, Type: core.NodeElement, Name: "html",
					Kids: []*core.Node{
						{
							ID: 3, Type: core.NodeElement, Name: "body",
							Kids: []*core.Node{
								{
									ID: 4, Type: core.NodeElement, Name: "div",
									Attrs: []core.Attr{{Name: "id", Value: "root"}},
									Kids: []*core.Node{
										{
											ID: 5, Type: core.NodeElement, Name: "p",
											Kids: []*core.Node{{ID: 6, Type: core.NodeText, Value: "hello"}},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func snapshotted(t *testing.T) *Mirror {
	t.Helper()
	m := newMirror("s_1")
	m.ApplySnapshot(MainFrame, doc(), 1)
	return m
}

func TestSnapshotThenRead(t *testing.T) {
	m := snapshotted(t)

	got, ok := m.Document()
	if !ok {
		t.Fatal("Document reported no mirror right after a snapshot")
	}
	if got.SessionID != "s_1" || got.FrameID != MainFrame {
		t.Errorf("document identity = %+v", got)
	}
	if got.Seq != 1 {
		t.Errorf("Seq = %d, want 1", got.Seq)
	}
	if got.Title != "Example" {
		t.Errorf("Title = %q", got.Title)
	}
}

func TestMutationsApplyInOrder(t *testing.T) {
	m := snapshotted(t)

	if err := m.ApplyMutations(2, []Op{
		{Kind: OpAttr, ID: 4, Name: "class", Value: str("live")},
		{Kind: OpText, ID: 6, Value: str("goodbye")},
	}); err != nil {
		t.Fatalf("ApplyMutations: %v", err)
	}

	html := renderMirror(t, m)
	if !strings.Contains(html, `class="live"`) {
		t.Errorf("attribute mutation not applied: %s", html)
	}
	if !strings.Contains(html, "goodbye") || strings.Contains(html, "hello") {
		t.Errorf("text mutation not applied: %s", html)
	}
	if m.Seq() != 2 {
		t.Errorf("Seq = %d, want 2", m.Seq())
	}
}

func TestInsertAndRemove(t *testing.T) {
	m := snapshotted(t)

	err := m.ApplyMutations(2, []Op{{
		Kind: OpInsert, Parent: 4, Ref: 5,
		Node: &core.Node{ID: 7, Type: core.NodeElement, Name: "span",
			Kids: []*core.Node{{ID: 8, Type: core.NodeText, Value: "first"}}},
	}})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	html := renderMirror(t, m)
	spanAt, pAt := strings.Index(html, "<span>"), strings.Index(html, "<p>")
	if spanAt < 0 || pAt < 0 {
		t.Fatalf("insert produced %s", html)
	}
	if spanAt > pAt {
		t.Errorf("node was appended rather than inserted before its ref: %s", html)
	}

	if err := m.ApplyMutations(3, []Op{{Kind: OpRemove, ID: 5}}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	html = renderMirror(t, m)
	if strings.Contains(html, "<p>") {
		t.Errorf("removed node is still present: %s", html)
	}
	if !strings.Contains(html, "<span>") {
		t.Errorf("remove took the wrong node: %s", html)
	}
}

func TestInsertWithoutRefAppends(t *testing.T) {
	m := snapshotted(t)
	if err := m.ApplyMutations(2, []Op{{
		Kind: OpInsert, Parent: 4,
		Node: &core.Node{ID: 7, Type: core.NodeElement, Name: "span"},
	}}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	html := renderMirror(t, m)
	if strings.Index(html, "<span>") < strings.Index(html, "<p>") {
		t.Errorf("append put the node before existing children: %s", html)
	}
}

func TestRemoveAttribute(t *testing.T) {
	m := snapshotted(t)
	if err := m.ApplyMutations(2, []Op{{Kind: OpAttr, ID: 4, Name: "id", Value: nil}}); err != nil {
		t.Fatalf("ApplyMutations: %v", err)
	}
	if html := renderMirror(t, m); strings.Contains(html, `id="root"`) {
		t.Errorf("attribute was not removed: %s", html)
	}
}

// This is the property that makes a mirror trustworthy rather than plausible.
// Deltas that would close the hole are gone, so the only correct answer is to
// stop serving the document and demand a fresh snapshot.
func TestSequenceGapMarksTheMirrorStale(t *testing.T) {
	m := snapshotted(t)

	err := m.ApplyMutations(5, []Op{{Kind: OpText, ID: 6, Value: str("skipped ahead")}})
	if !errors.Is(err, ErrSeqGap) {
		t.Fatalf("err = %v, want ErrSeqGap", err)
	}
	if !m.Stale() {
		t.Error("a gap did not mark the mirror stale")
	}
	if _, ok := m.Document(); ok {
		t.Error("a stale mirror still served a document, so a caller would read a silently wrong DOM")
	}
}

// A reconnecting agent may resend a batch it already delivered. Applying it
// twice would corrupt the tree, so duplicates must be ignored, not rejected.
func TestDuplicateBatchIsIgnored(t *testing.T) {
	m := snapshotted(t)

	ops := []Op{{
		Kind: OpInsert, Parent: 4,
		Node: &core.Node{ID: 7, Type: core.NodeElement, Name: "span"},
	}}
	if err := m.ApplyMutations(2, ops); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := m.ApplyMutations(2, ops); err != nil {
		t.Errorf("replaying a delivered batch returned %v, want nil", err)
	}
	if got := strings.Count(renderMirror(t, m), "<span>"); got != 1 {
		t.Errorf("span appears %d times, want 1: a duplicate batch was applied twice", got)
	}
	if m.Stale() {
		t.Error("a duplicate batch marked the mirror stale")
	}
}

func TestReSnapshotClearsStaleness(t *testing.T) {
	m := snapshotted(t)

	if err := m.ApplyMutations(9, nil); !errors.Is(err, ErrSeqGap) {
		t.Fatalf("err = %v, want ErrSeqGap", err)
	}
	if !m.Stale() {
		t.Fatal("expected a stale mirror")
	}

	m.ApplySnapshot(MainFrame, doc(), 9)
	if m.Stale() {
		t.Error("a re-snapshot did not clear staleness")
	}
	if _, ok := m.Document(); !ok {
		t.Error("no document after a re-snapshot")
	}
	if m.Seq() != 9 {
		t.Errorf("Seq = %d, want 9: a re-snapshot must adopt the agent's sequence", m.Seq())
	}
}

// Removing a node that is already gone leaves the same end state, so it is not
// a divergence and must not trip the gap detector.
func TestRemovingAnAlreadyRemovedNodeIsFine(t *testing.T) {
	m := snapshotted(t)
	if err := m.ApplyMutations(2, []Op{{Kind: OpRemove, ID: 5}}); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := m.ApplyMutations(3, []Op{{Kind: OpRemove, ID: 5}}); err != nil {
		t.Errorf("second remove returned %v, want nil", err)
	}
	if m.Stale() {
		t.Error("removing an absent node marked the mirror stale")
	}
}

func TestMutationOnUnknownNodeIsAGap(t *testing.T) {
	cases := map[string]Op{
		"attr on unknown node":  {Kind: OpAttr, ID: 999, Name: "x", Value: str("y")},
		"text on unknown node":  {Kind: OpText, ID: 999, Value: str("y")},
		"insert into unknown":   {Kind: OpInsert, Parent: 999, Node: &core.Node{ID: 1000}},
		"unknown mutation kind": {Kind: "teleport", ID: 4},
		"insert without a node": {Kind: OpInsert, Parent: 4},
	}
	for name, op := range cases {
		t.Run(name, func(t *testing.T) {
			m := snapshotted(t)
			if err := m.ApplyMutations(2, []Op{op}); !errors.Is(err, ErrSeqGap) {
				t.Errorf("err = %v, want ErrSeqGap", err)
			}
			if !m.Stale() {
				t.Error("mirror was not marked stale")
			}
		})
	}
}

func TestMutationsBeforeAnySnapshot(t *testing.T) {
	m := newMirror("s_1")
	if err := m.ApplyMutations(1, []Op{{Kind: OpText, ID: 6, Value: str("x")}}); !errors.Is(err, ErrNoMirror) {
		t.Errorf("err = %v, want ErrNoMirror", err)
	}
	if _, ok := m.Document(); ok {
		t.Error("Document served something before any snapshot")
	}
}

func TestChildFramesAreNested(t *testing.T) {
	m := snapshotted(t)
	m.ApplySnapshot("frame-7", core.Document{
		URL:  "https://embed.example.test/",
		Root: &core.Node{ID: 100, Type: core.NodeDocument},
	}, 1)

	got, ok := m.Document()
	if !ok {
		t.Fatal("no document")
	}
	if len(got.Frames) != 1 {
		t.Fatalf("got %d child frames, want 1", len(got.Frames))
	}
	if got.Frames[0].FrameID != "frame-7" || got.Frames[0].URL != "https://embed.example.test/" {
		t.Errorf("child frame = %+v", got.Frames[0])
	}
}

// A mutation for a frame that was never snapshotted has no tree to apply to,
// which is the same situation as a gap.
func TestMutationForUnknownFrameIsAGap(t *testing.T) {
	m := snapshotted(t)
	err := m.ApplyMutations(2, []Op{{Kind: OpText, Frame: "frame-never-seen", ID: 6, Value: str("x")}})
	if !errors.Is(err, ErrSeqGap) {
		t.Errorf("err = %v, want ErrSeqGap", err)
	}
}

func TestMarkStale(t *testing.T) {
	m := snapshotted(t)
	if _, ok := m.Document(); !ok {
		t.Fatal("expected a document")
	}
	m.MarkStale()
	if _, ok := m.Document(); ok {
		t.Error("MarkStale did not stop the mirror serving the old document")
	}
}

// ------------------------------------------------------------------ manager

func TestManagerLifecycle(t *testing.T) {
	mgr := NewManager(0)

	if _, ok := mgr.Lookup("s_1"); ok {
		t.Error("Lookup invented a mirror")
	}
	if _, ok := mgr.Document("s_1"); ok {
		t.Error("Document invented a mirror")
	}

	m := mgr.For("s_1")
	m.ApplySnapshot(MainFrame, doc(), 1)

	if got, ok := mgr.Document("s_1"); !ok || got.SessionID != "s_1" {
		t.Errorf("Document = %+v, %v", got, ok)
	}
	if mgr.For("s_1") != m {
		t.Error("For returned a different mirror for the same session")
	}

	mgr.Drop("s_1")
	if _, ok := mgr.Lookup("s_1"); ok {
		t.Error("Drop did not release the mirror")
	}
}

// A hub that has seen thousands of sessions must not hold every DOM it ever
// saw. Eviction is cheap here: a missing mirror costs one round trip, not data.
func TestManagerEvictsWhenFull(t *testing.T) {
	mgr := NewManager(3)

	for _, id := range []string{"s_1", "s_2", "s_3"} {
		mgr.For(id).ApplySnapshot(MainFrame, doc(), 1)
	}
	if mgr.Len() != 3 {
		t.Fatalf("Len = %d, want 3", mgr.Len())
	}

	mgr.For("s_4")
	if mgr.Len() != 3 {
		t.Errorf("Len = %d after exceeding the cap, want 3", mgr.Len())
	}
	if _, ok := mgr.Lookup("s_4"); !ok {
		t.Error("the newest session was not retained")
	}
}

func renderMirror(t *testing.T, m *Mirror) string {
	t.Helper()
	d, ok := m.Document()
	if !ok {
		t.Fatal("mirror has no document")
	}
	return RenderHTML(d)
}
