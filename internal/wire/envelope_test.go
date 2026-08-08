package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	in := NewCmd("c_01ABC", "s_01XYZ", "navigate", json.RawMessage(`{"url":"https://example.test"}`))
	b, err := Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != in.ID || out.SID != in.SID || out.Op != in.Op || out.Kind != KindCmd {
		t.Errorf("round trip changed the envelope: %+v", out)
	}
	if string(out.Body) != `{"url":"https://example.test"}` {
		t.Errorf("body = %s", out.Body)
	}
}

// The wire format is documented in docs/protocol.md and implemented separately
// in JavaScript. If a field name changes here, every agent in the field breaks,
// so the names are pinned by a test rather than by convention.
func TestEnvelopeJSONFieldNames(t *testing.T) {
	b, err := Encode(Envelope{
		V:    1,
		ID:   "c_1",
		Kind: KindEvt,
		TS:   1712345678901,
		SID:  "s_1",
		Op:   "navigated",
		Seq:  7,
		Body: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"v", "id", "t", "ts", "sid", "op", "seq", "body"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("field %q missing from the encoded envelope: %s", key, b)
		}
	}
	if len(raw) != 8 {
		t.Errorf("encoded %d fields, want 8: %s", len(raw), b)
	}
}

// Optional fields must not appear when empty: the mutation stream is mostly
// envelopes, and every omitted key is bytes off a busy page's event volume.
func TestEnvelopeOmitsEmptyFields(t *testing.T) {
	b, err := Encode(Envelope{V: 1, Kind: KindEvt, Op: "ping", TS: 1})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, key := range []string{`"id"`, `"sid"`, `"seq"`, `"body"`, `"err"`} {
		if strings.Contains(string(b), key) {
			t.Errorf("empty field %s was encoded: %s", key, b)
		}
	}
}

func TestValidateRejectsUnroutableEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		env  Envelope
	}{
		{"wrong version", Envelope{V: 2, Kind: KindEvt, Op: "x"}},
		{"zero version", Envelope{Kind: KindEvt, Op: "x"}},
		{"unknown kind", Envelope{V: 1, Kind: "shout", Op: "x"}},
		{"empty kind", Envelope{V: 1, Op: "x"}},
		{"cmd without id", Envelope{V: 1, Kind: KindCmd, Op: "navigate"}},
		{"cmd without op", Envelope{V: 1, Kind: KindCmd, ID: "c_1"}},
		{"res without id", Envelope{V: 1, Kind: KindRes}},
		{"err without id", Envelope{V: 1, Kind: KindErr, Err: &EnvelopeError{Code: "x"}}},
		{"err without an error", Envelope{V: 1, Kind: KindErr, ID: "c_1"}},
		{"err without a code", Envelope{V: 1, Kind: KindErr, ID: "c_1", Err: &EnvelopeError{Message: "m"}}},
		{"evt without op", Envelope{V: 1, Kind: KindEvt}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.env.Validate()
			if err == nil {
				t.Fatal("accepted an unroutable envelope")
			}
			if !IsBadEnvelope(err) {
				t.Errorf("err = %v, want it to classify as a bad envelope", err)
			}
		})
	}
}

func TestValidateAcceptsWellFormed(t *testing.T) {
	cases := []Envelope{
		NewCmd("c_1", "s_1", "navigate", nil),
		NewRes("c_1", "s_1", json.RawMessage(`{"ok":true}`)),
		NewErr("c_1", "s_1", CodeUnsupported, "this agent cannot eval"),
		NewEvt("s_1", "navigated", 3, json.RawMessage(`{"url":"x"}`)),
	}
	for _, e := range cases {
		if err := e.Validate(); err != nil {
			t.Errorf("%s envelope rejected: %v", e.Kind, err)
		}
	}
}

// A command with no session id is legal: agent-scoped commands (open a tab,
// list tabs) are not addressed to a session.
func TestCmdWithoutSessionIsLegal(t *testing.T) {
	if err := NewCmd("c_1", "", "openTab", nil).Validate(); err != nil {
		t.Errorf("agent-scoped command rejected: %v", err)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "{", "null", `{"v":1`, "[]", `"string"`} {
		if _, err := Decode([]byte(in)); err == nil {
			t.Errorf("Decode(%q) succeeded", in)
		} else if !IsBadEnvelope(err) {
			t.Errorf("Decode(%q) err = %v, want a bad-envelope classification", in, err)
		}
	}
}

func TestEnvelopeTime(t *testing.T) {
	e := Envelope{TS: 1712345678901}
	if got := e.Time().UnixMilli(); got != 1712345678901 {
		t.Errorf("Time() = %d, want the millisecond timestamp back", got)
	}
}

func TestEnvelopeErrorMessage(t *testing.T) {
	e := &EnvelopeError{Code: CodeNoSuchTab, Message: "tab 42 is gone"}
	if got, want := e.Error(), "no_such_tab: tab 42 is gone"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
