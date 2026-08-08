// Package mirror maintains a materialized copy of each session's document.
//
// A snapshot is a photograph; a mirror is a video feed with a photograph at the
// start. Keeping the document hub-side is what makes GET /v1/sessions/{id}/dom
// answer with no round trip to the browser -- and the reason the distinction is
// worth the machinery is that a scraper polling the DOM would otherwise put a
// command on the wire for every read.
//
// The invariant that makes a mirror trustworthy is that it can never be
// silently wrong: every mutation batch carries a sequence number, and a gap
// forces a re-snapshot rather than applying deltas to a document that has
// already diverged.
package mirror

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tayyebi/scraper/internal/core"
)

// MainFrame is the frame id of a session's top-level document. Child frames
// carry the browser's own frame ids.
const MainFrame = "main"

// ErrSeqGap reports that mutations arrived out of order or with a hole in them.
//
// It is not recoverable by retrying: the deltas that would have closed the gap
// are gone. The only correct response is to demand a fresh snapshot, which is
// what the Agent Plane does when it sees this.
var ErrSeqGap = errors.New("mutation sequence gap")

// ErrNoMirror reports that nothing has been snapshotted for a session yet.
var ErrNoMirror = errors.New("no mirror for this session")

// Mutation op kinds.
const (
	OpInsert = "insert"
	OpRemove = "remove"
	OpAttr   = "attr"
	OpText   = "text"
)

// Op is one mutation. Field names are short because a busy page's mutation
// stream is almost entirely this struct, repeated thousands of times.
type Op struct {
	Kind   string     `json:"op"`
	Frame  string     `json:"f,omitempty"`
	ID     int64      `json:"id,omitempty"`
	Parent int64      `json:"parent,omitempty"`
	Ref    int64      `json:"ref,omitempty"`
	Name   string     `json:"n,omitempty"`
	Value  *string    `json:"v,omitempty"`
	Node   *core.Node `json:"node,omitempty"`
}

// frameState is one frame's tree plus the indexes that make mutations O(1).
//
// Without the indexes every mutation would be a tree walk, and a page that
// mutates a thousand nodes per second would spend the hub's CPU on lookups.
type frameState struct {
	doc    core.Document
	index  map[int64]*core.Node
	parent map[int64]int64
}

// Mirror is one session's materialized document set.
type Mirror struct {
	mu sync.RWMutex

	sessionID string
	frames    map[string]*frameState
	seq       int64
	stale     bool
	updatedAt time.Time
}

func newMirror(sessionID string) *Mirror {
	return &Mirror{
		sessionID: sessionID,
		frames:    make(map[string]*frameState),
	}
}

// ApplySnapshot replaces a frame's document wholesale and clears staleness.
func (m *Mirror) ApplySnapshot(frameID string, doc core.Document, seq int64) {
	if frameID == "" {
		frameID = MainFrame
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	fs := &frameState{
		doc:    doc,
		index:  make(map[int64]*core.Node),
		parent: make(map[int64]int64),
	}
	fs.doc.FrameID = frameID
	fs.doc.SessionID = m.sessionID
	fs.doc.Seq = seq
	if fs.doc.CapturedAt.IsZero() {
		fs.doc.CapturedAt = time.Now().UTC()
	}
	indexTree(fs, fs.doc.Root, 0)

	m.frames[frameID] = fs
	m.seq = seq
	m.stale = false
	m.updatedAt = time.Now().UTC()
}

func indexTree(fs *frameState, n *core.Node, parentID int64) {
	if n == nil {
		return
	}
	fs.index[n.ID] = n
	if parentID != 0 {
		fs.parent[n.ID] = parentID
	}
	for _, kid := range n.Kids {
		indexTree(fs, kid, n.ID)
	}
	if n.Shadow != nil {
		indexTree(fs, n.Shadow, n.ID)
	}
}

// ApplyMutations applies one sequenced batch.
//
// seq is the batch's own number and must be exactly one past the last applied
// batch. Anything higher means deltas were lost; anything lower or equal is a
// duplicate from a reconnecting agent and is ignored.
func (m *Mirror) ApplyMutations(seq int64, ops []Op) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.frames) == 0 {
		return ErrNoMirror
	}
	if seq <= m.seq {
		// A resend, not a problem. Applying it twice would corrupt the tree.
		return nil
	}
	if seq != m.seq+1 {
		m.stale = true
		return fmt.Errorf("%w: expected seq %d, got %d", ErrSeqGap, m.seq+1, seq)
	}

	for _, op := range ops {
		frameID := op.Frame
		if frameID == "" {
			frameID = MainFrame
		}
		fs, ok := m.frames[frameID]
		if !ok {
			// A mutation for a frame we never snapshotted. The tree for it does
			// not exist, so this is the same situation as a gap.
			m.stale = true
			return fmt.Errorf("%w: mutation for unsnapshotted frame %q", ErrSeqGap, frameID)
		}
		if err := fs.apply(op); err != nil {
			m.stale = true
			return err
		}
	}

	m.seq = seq
	m.updatedAt = time.Now().UTC()
	for _, fs := range m.frames {
		fs.doc.Seq = seq
	}
	return nil
}

func (fs *frameState) apply(op Op) error {
	switch op.Kind {
	case OpAttr:
		n, ok := fs.index[op.ID]
		if !ok {
			return fmt.Errorf("%w: attr on unknown node %d", ErrSeqGap, op.ID)
		}
		setAttr(n, op.Name, op.Value)
		return nil

	case OpText:
		n, ok := fs.index[op.ID]
		if !ok {
			return fmt.Errorf("%w: text on unknown node %d", ErrSeqGap, op.ID)
		}
		if op.Value != nil {
			n.Value = *op.Value
		}
		return nil

	case OpInsert:
		if op.Node == nil {
			return fmt.Errorf("%w: insert without a node", ErrSeqGap)
		}
		parent, ok := fs.index[op.Parent]
		if !ok {
			return fmt.Errorf("%w: insert into unknown parent %d", ErrSeqGap, op.Parent)
		}
		insertChild(parent, op.Node, op.Ref)
		indexTree(fs, op.Node, parent.ID)
		return nil

	case OpRemove:
		n, ok := fs.index[op.ID]
		if !ok {
			// Already gone. The end state matches, so this is not a divergence.
			return nil
		}
		parentID, ok := fs.parent[op.ID]
		if !ok {
			return fmt.Errorf("%w: cannot remove the root node %d", ErrSeqGap, op.ID)
		}
		parent, ok := fs.index[parentID]
		if !ok {
			return fmt.Errorf("%w: remove from unknown parent %d", ErrSeqGap, parentID)
		}
		removeChild(parent, op.ID)
		unindexTree(fs, n)
		return nil

	default:
		return fmt.Errorf("%w: unknown mutation %q", ErrSeqGap, op.Kind)
	}
}

func setAttr(n *core.Node, name string, value *string) {
	if value == nil {
		for i, a := range n.Attrs {
			if a.Name == name {
				n.Attrs = append(n.Attrs[:i], n.Attrs[i+1:]...)
				return
			}
		}
		return
	}
	for i, a := range n.Attrs {
		if a.Name == name {
			n.Attrs[i].Value = *value
			return
		}
	}
	n.Attrs = append(n.Attrs, core.Attr{Name: name, Value: *value})
}

// insertChild places node before ref, or appends when ref is 0 or absent.
func insertChild(parent *core.Node, node *core.Node, ref int64) {
	if ref != 0 {
		for i, kid := range parent.Kids {
			if kid.ID == ref {
				parent.Kids = append(parent.Kids[:i], append([]*core.Node{node}, parent.Kids[i:]...)...)
				return
			}
		}
	}
	parent.Kids = append(parent.Kids, node)
}

func removeChild(parent *core.Node, id int64) {
	for i, kid := range parent.Kids {
		if kid.ID == id {
			parent.Kids = append(parent.Kids[:i], parent.Kids[i+1:]...)
			return
		}
	}
	if parent.Shadow != nil && parent.Shadow.ID == id {
		parent.Shadow = nil
	}
}

func unindexTree(fs *frameState, n *core.Node) {
	if n == nil {
		return
	}
	delete(fs.index, n.ID)
	delete(fs.parent, n.ID)
	for _, kid := range n.Kids {
		unindexTree(fs, kid)
	}
	unindexTree(fs, n.Shadow)
}

// Document assembles the session's document: the main frame with child frames
// nested underneath it.
func (m *Mirror) Document() (core.Document, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	main, ok := m.frames[MainFrame]
	if !ok || m.stale {
		return core.Document{}, false
	}

	doc := main.doc
	doc.Frames = nil
	for id, fs := range m.frames {
		if id == MainFrame {
			continue
		}
		child := fs.doc
		doc.Frames = append(doc.Frames, &child)
	}
	return doc, true
}

// Stale reports whether the mirror has diverged and needs a re-snapshot.
func (m *Mirror) Stale() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stale || len(m.frames) == 0
}

// Seq reports the last applied mutation sequence number.
func (m *Mirror) Seq() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.seq
}

// MarkStale forces the next read to miss, so a re-snapshot is demanded. The
// Agent Plane calls it when a session navigates: the old document is gone.
func (m *Mirror) MarkStale() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stale = true
}

// ------------------------------------------------------------------ manager

// Manager holds one Mirror per session and implements registry.DocumentSource.
type Manager struct {
	mu      sync.RWMutex
	mirrors map[string]*Mirror

	// max bounds how many sessions keep a materialized document. A hub with
	// thousands of historical sessions must not hold every DOM it ever saw.
	max int
}

// DefaultMaxMirrors is how many sessions keep a live mirror.
const DefaultMaxMirrors = 512

// NewManager returns an empty Manager.
func NewManager(max int) *Manager {
	if max <= 0 {
		max = DefaultMaxMirrors
	}
	return &Manager{mirrors: make(map[string]*Mirror), max: max}
}

// For returns the session's mirror, creating it if needed.
func (mgr *Manager) For(sessionID string) *Mirror {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	if m, ok := mgr.mirrors[sessionID]; ok {
		return m
	}
	if len(mgr.mirrors) >= mgr.max {
		mgr.evictOldestLocked()
	}
	m := newMirror(sessionID)
	mgr.mirrors[sessionID] = m
	return m
}

// evictOldestLocked drops the least recently updated mirror. Evicting is safe
// in a way that dropping most caches is not: a missing mirror costs one round
// trip to rebuild, it does not lose data.
func (mgr *Manager) evictOldestLocked() {
	var (
		oldestID string
		oldest   time.Time
	)
	for id, m := range mgr.mirrors {
		m.mu.RLock()
		updated := m.updatedAt
		m.mu.RUnlock()
		if oldestID == "" || updated.Before(oldest) {
			oldestID, oldest = id, updated
		}
	}
	if oldestID != "" {
		delete(mgr.mirrors, oldestID)
	}
}

// Lookup returns an existing mirror without creating one.
func (mgr *Manager) Lookup(sessionID string) (*Mirror, bool) {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	m, ok := mgr.mirrors[sessionID]
	return m, ok
}

// Document implements registry.DocumentSource.
func (mgr *Manager) Document(sessionID string) (core.Document, bool) {
	m, ok := mgr.Lookup(sessionID)
	if !ok {
		return core.Document{}, false
	}
	return m.Document()
}

// Drop releases a session's mirror, on session close.
func (mgr *Manager) Drop(sessionID string) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	delete(mgr.mirrors, sessionID)
}

// Len reports how many sessions currently hold a mirror.
func (mgr *Manager) Len() int {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	return len(mgr.mirrors)
}
