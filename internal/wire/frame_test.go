package wire

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

// The vectors in RFC 6455 section 5.7, verbatim. These are the ground truth for
// the frame codec: if the hand-rolled implementation disagrees with them it
// disagrees with every browser.
var rfcVectors = []struct {
	name    string
	raw     []byte
	fin     bool
	opcode  Opcode
	masked  bool
	payload []byte
}{
	{
		name:    "single-frame unmasked text",
		raw:     []byte{0x81, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f},
		fin:     true,
		opcode:  OpText,
		payload: []byte("Hello"),
	},
	{
		name:    "single-frame masked text",
		raw:     []byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58},
		fin:     true,
		opcode:  OpText,
		masked:  true,
		payload: []byte("Hello"),
	},
	{
		name:    "fragmented text, first fragment",
		raw:     []byte{0x01, 0x03, 0x48, 0x65, 0x6c},
		fin:     false,
		opcode:  OpText,
		payload: []byte("Hel"),
	},
	{
		name:    "fragmented text, final fragment",
		raw:     []byte{0x80, 0x02, 0x6c, 0x6f},
		fin:     true,
		opcode:  OpContinuation,
		payload: []byte("lo"),
	},
	{
		name:    "unmasked ping",
		raw:     []byte{0x89, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f},
		fin:     true,
		opcode:  OpPing,
		payload: []byte("Hello"),
	},
	{
		name:    "masked pong",
		raw:     []byte{0x8a, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58},
		fin:     true,
		opcode:  OpPong,
		masked:  true,
		payload: []byte("Hello"),
	},
}

func TestReadHeaderRFCVectors(t *testing.T) {
	for _, v := range rfcVectors {
		t.Run(v.name, func(t *testing.T) {
			br := bufio.NewReader(bytes.NewReader(v.raw))
			h, err := readHeader(br)
			if err != nil {
				t.Fatalf("readHeader: %v", err)
			}
			if h.fin != v.fin {
				t.Errorf("fin = %v, want %v", h.fin, v.fin)
			}
			if h.opcode != v.opcode {
				t.Errorf("opcode = %s, want %s", h.opcode, v.opcode)
			}
			if h.masked != v.masked {
				t.Errorf("masked = %v, want %v", h.masked, v.masked)
			}
			if h.length != int64(len(v.payload)) {
				t.Fatalf("length = %d, want %d", h.length, len(v.payload))
			}
			body := make([]byte, h.length)
			if _, err := io.ReadFull(br, body); err != nil {
				t.Fatalf("read payload: %v", err)
			}
			if h.masked {
				maskBytes(h.maskKey, 0, body)
			}
			if !bytes.Equal(body, v.payload) {
				t.Errorf("payload = %q, want %q", body, v.payload)
			}
		})
	}
}

func TestWriteFrameMatchesRFCVectors(t *testing.T) {
	for _, v := range rfcVectors {
		if v.masked {
			// Masking keys are random, so the exact bytes are only reproducible
			// for the unmasked vectors. Masked ones are covered by the
			// round-trip test below.
			continue
		}
		t.Run(v.name, func(t *testing.T) {
			var buf bytes.Buffer
			bw := bufio.NewWriter(&buf)
			if err := writeFrame(bw, v.fin, v.opcode, nil, v.payload); err != nil {
				t.Fatalf("writeFrame: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), v.raw) {
				t.Errorf("frame = % x\n want % x", buf.Bytes(), v.raw)
			}
		})
	}
}

// The RFC's two extended-length vectors: 256 bytes uses the 16-bit form, 64 KiB
// uses the 64-bit form. Getting the boundary wrong here is the classic
// hand-rolled-WebSocket bug.
func TestExtendedLengthEncoding(t *testing.T) {
	cases := []struct {
		size   int
		prefix []byte
	}{
		{125, []byte{0x82, 0x7d}},
		{126, []byte{0x82, 0x7e, 0x00, 0x7e}},
		{256, []byte{0x82, 0x7e, 0x01, 0x00}},
		{65535, []byte{0x82, 0x7e, 0xff, 0xff}},
		{65536, []byte{0x82, 0x7f, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00}},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := writeFrame(bw, true, OpBinary, nil, make([]byte, c.size)); err != nil {
			t.Fatalf("size %d: writeFrame: %v", c.size, err)
		}
		got := buf.Bytes()
		if len(got) != len(c.prefix)+c.size {
			t.Errorf("size %d: frame is %d bytes, want %d", c.size, len(got), len(c.prefix)+c.size)
		}
		if !bytes.Equal(got[:len(c.prefix)], c.prefix) {
			t.Errorf("size %d: header = % x, want % x", c.size, got[:len(c.prefix)], c.prefix)
		}

		br := bufio.NewReader(bytes.NewReader(got))
		h, err := readHeader(br)
		if err != nil {
			t.Fatalf("size %d: readHeader: %v", c.size, err)
		}
		if h.length != int64(c.size) {
			t.Errorf("size %d: decoded length %d", c.size, h.length)
		}
	}
}

// A peer that encodes a small length in a large field is either broken or is
// trying to make us and a proxy disagree about where the frame ends.
func TestNonMinimalLengthRejected(t *testing.T) {
	cases := map[string][]byte{
		"16-bit field holding 5":  {0x82, 0x7e, 0x00, 0x05},
		"64-bit field holding 5":  {0x82, 0x7f, 0, 0, 0, 0, 0, 0, 0, 0x05},
		"64-bit field holding 1k": {0x82, 0x7f, 0, 0, 0, 0, 0, 0, 0x03, 0xe8},
	}
	for name, raw := range cases {
		br := bufio.NewReader(bytes.NewReader(raw))
		_, err := readHeader(br)
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			t.Errorf("%s: err = %v, want a ProtocolError", name, err)
			continue
		}
		if pe.Code != CloseProtocolError {
			t.Errorf("%s: close code = %d, want %d", name, pe.Code, CloseProtocolError)
		}
	}
}

func TestMaskIsSymmetric(t *testing.T) {
	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	original := []byte("the quick brown fox jumps over the lazy dog")

	b := append([]byte(nil), original...)
	maskBytes(key, 0, b)
	if bytes.Equal(b, original) {
		t.Fatal("masking left the payload unchanged")
	}
	maskBytes(key, 0, b)
	if !bytes.Equal(b, original) {
		t.Errorf("unmask round trip = %q, want %q", b, original)
	}
}

// Masking must be resumable across buffer boundaries, or a payload split across
// reads decodes to garbage after the first chunk.
func TestMaskResumesAcrossChunks(t *testing.T) {
	key := [4]byte{0x01, 0x02, 0x03, 0x04}
	original := []byte("abcdefghijklmnopqrstuvwxyz")

	whole := append([]byte(nil), original...)
	maskBytes(key, 0, whole)

	chunked := append([]byte(nil), original...)
	pos := 0
	for i := 0; i < len(chunked); i += 7 {
		end := min(i+7, len(chunked))
		pos = maskBytes(key, pos, chunked[i:end])
	}
	if !bytes.Equal(whole, chunked) {
		t.Errorf("chunked masking = % x\n           want % x", chunked, whole)
	}
}

func TestWriteFrameDoesNotMutateCallerBuffer(t *testing.T) {
	payload := []byte("Hello")
	original := append([]byte(nil), payload...)
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	key := [4]byte{1, 2, 3, 4}
	if err := writeFrame(bw, true, OpText, &key, payload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	if !bytes.Equal(payload, original) {
		t.Errorf("caller buffer was masked in place: %q, want %q", payload, original)
	}
}

func TestParseClose(t *testing.T) {
	t.Run("empty means no status", func(t *testing.T) {
		code, reason, err := parseClose(nil)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if code != CloseNoStatus || reason != "" {
			t.Errorf("got %d %q, want %d \"\"", code, reason, CloseNoStatus)
		}
	})
	t.Run("code and reason", func(t *testing.T) {
		code, reason, err := parseClose([]byte{0x03, 0xe8, 'b', 'y', 'e'})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if code != CloseNormal || reason != "bye" {
			t.Errorf("got %d %q, want 1000 \"bye\"", code, reason)
		}
	})
	t.Run("one byte is malformed", func(t *testing.T) {
		if _, _, err := parseClose([]byte{0x03}); err == nil {
			t.Error("accepted a 1-byte close payload")
		}
	})
	t.Run("reserved codes rejected", func(t *testing.T) {
		for _, code := range []int{999, 1004, 1005, 1006, 1012, 1999, 2999, 5000} {
			payload := []byte{byte(code >> 8), byte(code)}
			if _, _, err := parseClose(payload); err == nil {
				t.Errorf("accepted reserved close code %d", code)
			}
		}
	})
	t.Run("application codes accepted", func(t *testing.T) {
		for _, code := range []int{1000, 1001, 1002, 1003, 1007, 1011, 3000, 4000, 4999} {
			payload := []byte{byte(code >> 8), byte(code)}
			if _, _, err := parseClose(payload); err != nil {
				t.Errorf("rejected valid close code %d: %v", code, err)
			}
		}
	})
}

// 1005 and 1006 are what the application sees, never what goes on the wire.
func TestClosePayloadOmitsNonTransmissibleCodes(t *testing.T) {
	for _, code := range []int{CloseNoStatus, CloseAbnormal, 0} {
		if got := closePayload(code, "reason"); got != nil {
			t.Errorf("closePayload(%d) = % x, want nil", code, got)
		}
	}
	if got := closePayload(CloseNormal, "bye"); !bytes.Equal(got, []byte{0x03, 0xe8, 'b', 'y', 'e'}) {
		t.Errorf("closePayload(1000, bye) = % x", got)
	}
}

// A control frame carries at most 125 bytes including the 2-byte code, so a
// long reason must be truncated rather than producing an illegal frame.
func TestClosePayloadTruncatesLongReason(t *testing.T) {
	got := closePayload(ClosePolicyViolation, string(bytes.Repeat([]byte("x"), 500)))
	if len(got) > 125 {
		t.Errorf("close payload is %d bytes, exceeds the 125-byte control frame limit", len(got))
	}
}

func TestOpcodeIsControl(t *testing.T) {
	for _, op := range []Opcode{OpClose, OpPing, OpPong} {
		if !op.IsControl() {
			t.Errorf("%s must be a control frame", op)
		}
	}
	for _, op := range []Opcode{OpContinuation, OpText, OpBinary} {
		if op.IsControl() {
			t.Errorf("%s must not be a control frame", op)
		}
	}
}

func TestIsCleanClose(t *testing.T) {
	if !IsCleanClose(&CloseError{Code: CloseNormal}) {
		t.Error("1000 is a clean close")
	}
	if !IsCleanClose(&CloseError{Code: CloseGoingAway}) {
		t.Error("1001 is a clean close: a laptop lid closing is not a fault")
	}
	if IsCleanClose(&CloseError{Code: CloseProtocolError}) {
		t.Error("1002 is not a clean close")
	}
	if IsCleanClose(errors.New("boom")) {
		t.Error("a non-close error is not a clean close")
	}
}
