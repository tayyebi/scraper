package core

import (
	"sort"
	"strings"
	"testing"
	"time"
)

func TestNewIDShapeAndPrefix(t *testing.T) {
	id := NewID(PrefixCommand)
	if !strings.HasPrefix(id, "c_") {
		t.Fatalf("id %q lacks type prefix", id)
	}
	if got, want := len(id), 2+26; got != want {
		t.Fatalf("id %q length = %d, want %d", id, got, want)
	}
	if !HasPrefix(id, PrefixCommand) {
		t.Errorf("HasPrefix(%q, %q) = false", id, PrefixCommand)
	}
	if HasPrefix(id, PrefixSession) {
		t.Errorf("command id %q accepted as a session id", id)
	}
}

func TestHasPrefixRejectsMalformed(t *testing.T) {
	for _, id := range []string{
		"",
		"c",
		"c_",
		"_ABCDEFGHJKMNPQRSTVWXYZ0123",
		"c_TOOSHORT",
		"c_0123456789ABCDEFGHJKMNPQRSTVWXYZ", // too long
		"c_IIIIIIIIIIIIIIIIIIIIIIIIII",       // I is not in the alphabet
	} {
		if HasPrefix(id, PrefixCommand) {
			t.Errorf("HasPrefix(%q) = true, want false", id)
		}
	}
}

// Ids must sort chronologically, because pagination cursors depend on it.
func TestIDsSortChronologically(t *testing.T) {
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var ids []string
	for i := range 50 {
		ids = append(ids, newIDAt(PrefixEvent, base.Add(time.Duration(i)*time.Millisecond)))
	}
	shuffled := append([]string(nil), ids...)
	sort.Sort(sort.Reverse(sort.StringSlice(shuffled)))
	sort.Strings(shuffled)
	for i := range ids {
		if shuffled[i] != ids[i] {
			t.Fatalf("lexicographic sort does not match chronological order at %d:\n got %s\nwant %s", i, shuffled[i], ids[i])
		}
	}
}

func TestIDTimeRoundTrip(t *testing.T) {
	want := time.Date(2026, 8, 8, 9, 30, 15, 0, time.UTC)
	id := newIDAt(PrefixAgent, want)
	got, ok := IDTime(id)
	if !ok {
		t.Fatalf("IDTime(%q) failed", id)
	}
	if !got.Equal(want) {
		t.Errorf("IDTime = %s, want %s", got, want)
	}
	if _, ok := IDTime("not-an-id"); ok {
		t.Error("IDTime accepted a non-id")
	}
}

func TestIDsAreUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		id := NewID(PrefixSession)
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestScopeImplies(t *testing.T) {
	cases := []struct {
		have Scope
		want Scope
		ok   bool
	}{
		{ScopeAdmin, ScopeRead, true},
		{ScopeAdmin, ScopeSteer, true},
		{ScopeAdmin, ScopeAdmin, true},
		{ScopeSteer, ScopeRead, true},
		{ScopeSteer, ScopeSteer, true},
		{ScopeSteer, ScopeAdmin, false},
		{ScopeRead, ScopeRead, true},
		{ScopeRead, ScopeSteer, false},
		{ScopeRead, ScopeAdmin, false},
		{Scope("bogus"), ScopeRead, false},
		{ScopeAdmin, Scope("bogus"), false},
	}
	for _, c := range cases {
		if got := c.have.Implies(c.want); got != c.ok {
			t.Errorf("%q.Implies(%q) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

func TestCaptureFidelity(t *testing.T) {
	if CaptureFetchPatch.FullFidelity() {
		t.Error("fetch-patch must not claim full fidelity: it cannot see navigations")
	}
	if CaptureNone.FullFidelity() {
		t.Error("none must not claim full fidelity")
	}
	if !CaptureDebugger.FullFidelity() || !CaptureFilterResponse.FullFidelity() {
		t.Error("CDP and filterResponseData are full fidelity")
	}
}

// A partial capture mode must say what it misses. Returning an empty gap list
// for fetch-patch would be the exact "silently shortchanged" failure the
// capability reporting exists to prevent.
func TestCaptureGapsAreDeclared(t *testing.T) {
	if got := (Capabilities{Capture: CaptureFetchPatch}).CaptureGaps(); len(got) == 0 {
		t.Error("fetch-patch reported no capture gaps")
	}
	if got := (Capabilities{Capture: CaptureDebugger}).CaptureGaps(); len(got) != 0 {
		t.Errorf("full-fidelity mode reported gaps: %v", got)
	}
	if got := (Capabilities{Capture: CaptureNone}).CaptureGaps(); len(got) == 0 {
		t.Error("none reported no capture gaps")
	}
}

func TestCapabilitiesSupports(t *testing.T) {
	c := Capabilities{Ops: []string{OpNavigate, OpClick}}
	if !c.Supports(OpNavigate) {
		t.Error("advertised op reported unsupported")
	}
	if c.Supports(OpEval) {
		t.Error("unadvertised op reported supported")
	}
}

func TestKnownOps(t *testing.T) {
	if len(KnownOps) != 15 {
		t.Errorf("v1 vocabulary has %d ops, want 15", len(KnownOps))
	}
	seen := map[string]bool{}
	for _, op := range KnownOps {
		if seen[op] {
			t.Errorf("duplicate op %q", op)
		}
		seen[op] = true
		if !IsKnownOp(op) {
			t.Errorf("IsKnownOp(%q) = false", op)
		}
	}
	if IsKnownOp("rm -rf") {
		t.Error("IsKnownOp accepted an unknown op")
	}
}

func TestCommandStateTerminal(t *testing.T) {
	for _, s := range []CommandState{CommandDone, CommandError, CommandTimeout} {
		if !s.Terminal() {
			t.Errorf("%q must be terminal", s)
		}
	}
	for _, s := range []CommandState{CommandPending, CommandDispatched} {
		if s.Terminal() {
			t.Errorf("%q must not be terminal", s)
		}
	}
}

func TestEnrollmentTokenSpent(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	fresh := EnrollmentToken{ExpiresAt: now.Add(time.Hour)}
	if fresh.Spent(now) {
		t.Error("unused, unexpired token reported spent")
	}
	expired := EnrollmentToken{ExpiresAt: now.Add(-time.Second)}
	if !expired.Spent(now) {
		t.Error("expired token reported unspent")
	}
	used := now.Add(-time.Minute)
	consumed := EnrollmentToken{ExpiresAt: now.Add(time.Hour), UsedAt: &used}
	if !consumed.Spent(now) {
		t.Error("a one-time token stayed usable after being used")
	}
}
