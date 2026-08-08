package blob

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tayyebi/scraper/internal/core"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestPutAndOpen(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	body := []byte("<!doctype html><title>hello</title>")
	digest := Digest(body)

	info, err := s.Put(ctx, digest, bytes.NewReader(body), "text/html", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", info.Size, len(body))
	}
	if !s.Has(ctx, digest) {
		t.Error("Has = false right after Put")
	}

	r, got, err := s.Open(ctx, digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Errorf("content = %q, want %q", buf.Bytes(), body)
	}
	if got.Digest != digest {
		t.Errorf("digest = %q, want %q", got.Digest, digest)
	}
	if !strings.HasPrefix(got.MimeType, "text/html") {
		t.Errorf("sniffed mime = %q, want text/html", got.MimeType)
	}
}

// The name of a blob is its checksum. Storing bytes that hash to something else
// would make every later read return the wrong content under a trusted name.
func TestPutRejectsDigestMismatch(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	claimed := Digest([]byte("what the agent said it was uploading"))
	_, err := s.Put(ctx, claimed, bytes.NewReader([]byte("something else entirely")), "", 0)
	if !errors.Is(err, core.ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
	if s.Has(ctx, claimed) {
		t.Error("a mismatched upload was stored anyway")
	}

	// Nothing may be left in the temp directory either.
	entries, err := os.ReadDir(filepath.Join(s.tmpDir))
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d temp files left behind after a rejected upload", len(entries))
	}
}

func TestPutEnforcesSizeCap(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	body := bytes.Repeat([]byte("x"), 4096)
	_, err := s.Put(ctx, Digest(body), bytes.NewReader(body), "", 1024)
	if !errors.Is(err, core.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if s.Has(ctx, Digest(body)) {
		t.Error("an oversized upload was stored")
	}
}

// Content addressing is what makes a thousand captures of the same response
// cost one copy.
func TestPutDeduplicates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	body := []byte("identical bytes")
	digest := Digest(body)

	for range 5 {
		if _, err := s.Put(ctx, digest, bytes.NewReader(body), "", 0); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	total, count, err := s.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if count != 1 {
		t.Errorf("stored %d blobs, want 1", count)
	}
	if total != int64(len(body)) {
		t.Errorf("total = %d bytes, want %d", total, len(body))
	}
}

func TestConcurrentPutsOfSameDigest(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	body := bytes.Repeat([]byte("concurrent"), 1000)
	digest := Digest(body)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Put(ctx, digest, bytes.NewReader(body), "", 0); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Put: %v", err)
	}

	_, count, err := s.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if count != 1 {
		t.Errorf("stored %d blobs, want 1", count)
	}
}

// A digest becomes a filename, so path traversal has to be impossible by
// construction rather than by sanitizing.
func TestInvalidDigestsRejected(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	bad := []string{
		"",
		"short",
		"../../../../etc/passwd",
		strings.Repeat("g", 64), // not hex
		strings.Repeat("A", 64), // uppercase is not the canonical form
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		"a/b/" + strings.Repeat("c", 60),
	}
	for _, d := range bad {
		if ValidDigest(d) {
			t.Errorf("ValidDigest(%q) = true", d)
		}
		if _, err := s.Put(ctx, d, bytes.NewReader(nil), "", 0); err == nil {
			t.Errorf("Put accepted digest %q", d)
		}
		if _, _, err := s.Open(ctx, d); err == nil {
			t.Errorf("Open accepted digest %q", d)
		}
		if s.Has(ctx, d) {
			t.Errorf("Has accepted digest %q", d)
		}
	}
}

func TestOpenMissingIsNotFound(t *testing.T) {
	s := newStore(t)
	_, _, err := s.Open(context.Background(), Digest([]byte("never stored")))
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Deleting something already gone is the same end state, and retention races
// with refcount GC by nature.
func TestDeleteIsIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	body := []byte("temporary")
	digest := Digest(body)
	if _, err := s.Put(ctx, digest, bytes.NewReader(body), "", 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for range 3 {
		if err := s.Delete(ctx, digest); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}
	if s.Has(ctx, digest) {
		t.Error("blob survived deletion")
	}
}

func TestSweepByAge(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	old := []byte("old blob")
	fresh := []byte("fresh blob")
	oldDigest, freshDigest := Digest(old), Digest(fresh)

	for _, b := range [][]byte{old, fresh} {
		if _, err := s.Put(ctx, Digest(b), bytes.NewReader(b), "", 0); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Age the first blob by rewriting its mtime.
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(s.path(oldDigest), past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := s.Sweep(ctx, 24*time.Hour, 0, nil)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d blobs, want 1", removed)
	}
	if s.Has(ctx, oldDigest) {
		t.Error("the aged-out blob survived")
	}
	if !s.Has(ctx, freshDigest) {
		t.Error("the fresh blob was swept")
	}
}

// A referenced blob must survive retention regardless of age, or the request
// log ends up pointing at artifacts that no longer exist.
func TestSweepHonoursKeepPredicate(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	body := []byte("still referenced by an exchange")
	digest := Digest(body)
	if _, err := s.Put(ctx, digest, bytes.NewReader(body), "", 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	past := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(s.path(digest), past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := s.Sweep(ctx, time.Hour, 0, func(d string) bool { return d == digest })
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed %d blobs, want 0", removed)
	}
	if !s.Has(ctx, digest) {
		t.Error("a referenced blob was swept")
	}
}

func TestSweepBySizeRemovesOldestFirst(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Three distinct 1 KiB blobs, aged oldest to newest.
	var digests []string
	for i := range 3 {
		body := append(bytes.Repeat([]byte("a"), 1024), byte('0'+i))
		d := Digest(body)
		if _, err := s.Put(ctx, d, bytes.NewReader(body), "", 0); err != nil {
			t.Fatalf("Put: %v", err)
		}
		when := time.Now().Add(time.Duration(i-3) * time.Hour)
		if err := os.Chtimes(s.path(d), when, when); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		digests = append(digests, d)
	}

	// Room for roughly one blob.
	if _, err := s.Sweep(ctx, 0, 1200, nil); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	total, count, err := s.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if total > 1200 {
		t.Errorf("store still holds %d bytes, want <= 1200", total)
	}
	if count != 1 {
		t.Fatalf("kept %d blobs, want 1", count)
	}
	if !s.Has(ctx, digests[2]) {
		t.Error("size sweep kept an older blob instead of the newest")
	}
}

func TestNewClearsAbandonedTempFiles(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Simulate a process that died mid-upload.
	stale := filepath.Join(s.tmpDir, "up-crashed")
	if err := os.WriteFile(stale, []byte("partial"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := New(root); err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Error("a partial upload from a previous process was not cleaned up")
	}
}

func TestNormalizeDigest(t *testing.T) {
	want := strings.Repeat("ab", 32)
	for _, in := range []string{
		want,
		strings.ToUpper(want),
		"sha256:" + want,
		"  sha256:" + strings.ToUpper(want) + "  ",
	} {
		if got := NormalizeDigest(in); got != want {
			t.Errorf("NormalizeDigest(%q) = %q, want %q", in, got, want)
		}
	}
}
