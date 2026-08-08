// Package blob is the content-addressed artifact store.
//
// Captured response bodies are the one part of this system that grows without
// bound, so they are kept out of SQLite entirely and stored on disk under their
// own sha256. Content addressing is not decoration here: a scraper hitting the
// same endpoint a thousand times stores one copy, and an agent can ask whether
// the hub already has a digest and skip the upload.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tayyebi/scraper/internal/core"
)

// Store keeps blobs at <root>/blobs/ab/cd/<sha256>.
//
// The two-level fan-out exists because some filesystems degrade badly with
// hundreds of thousands of entries in one directory, and a busy hub reaches
// that in days.
type Store struct {
	root   string
	tmpDir string

	// mu serializes rename-into-place so two concurrent uploads of the same
	// digest cannot half-overwrite each other. Reads are not serialized.
	mu sync.Mutex
}

// New prepares the store directories under root.
func New(root string) (*Store, error) {
	s := &Store{
		root:   filepath.Join(root, "blobs"),
		tmpDir: filepath.Join(root, "blobs", ".tmp"),
	}
	if err := os.MkdirAll(s.tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("blob store: %w", err)
	}
	// Temp files from a previous process that died mid-upload are garbage; the
	// content-addressed name means nothing references them.
	if entries, err := os.ReadDir(s.tmpDir); err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(s.tmpDir, e.Name()))
		}
	}
	return s, nil
}

// ValidDigest reports whether s is a lowercase hex sha256.
//
// This is a path-safety check as much as a format check: the digest becomes a
// filename, so anything containing a separator or "." must never reach the
// filesystem.
func ValidDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *Store) path(digest string) string {
	return filepath.Join(s.root, digest[0:2], digest[2:4], digest)
}

// Put streams r into the store under digest, verifying the content matches.
//
// A mismatch stores nothing. The name of a blob is its checksum, so a blob that
// does not hash to its own name must never be allowed to exist -- otherwise
// every later read silently returns the wrong bytes under a trusted name.
func (s *Store) Put(ctx context.Context, digest string, r io.Reader, mimeType string, limit int64) (core.BlobInfo, error) {
	if !ValidDigest(digest) {
		return core.BlobInfo{}, fmt.Errorf("%w: %q is not a sha256 hex digest", core.ErrBadRequest, digest)
	}

	// Already present: content addressing means there is nothing to update.
	if info, err := s.Stat(ctx, digest); err == nil {
		_, _ = io.Copy(io.Discard, r)
		return info, nil
	}

	tmp, err := os.CreateTemp(s.tmpDir, "up-*")
	if err != nil {
		return core.BlobInfo{}, err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	src := r
	if limit > 0 {
		// limit+1 so exceeding the cap is detectable rather than silently
		// truncating the body and storing it under a digest it does not have.
		src = io.LimitReader(r, limit+1)
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		return core.BlobInfo{}, err
	}
	if limit > 0 && n > limit {
		return core.BlobInfo{}, fmt.Errorf("%w: body exceeds the %d byte cap", core.ErrTooLarge, limit)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return core.BlobInfo{}, fmt.Errorf("%w: content hashes to %s, not %s", core.ErrDigestMismatch, got, digest)
	}
	if err := tmp.Sync(); err != nil {
		return core.BlobInfo{}, err
	}
	if err := tmp.Close(); err != nil {
		return core.BlobInfo{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	final := s.path(digest)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return core.BlobInfo{}, err
	}
	if _, err := os.Stat(final); err == nil {
		// Someone won the race while we were hashing. Their bytes have the same
		// digest, so they are the same bytes.
		return s.Stat(ctx, digest)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return core.BlobInfo{}, err
	}

	return core.BlobInfo{
		Digest:    digest,
		Size:      n,
		MimeType:  mimeType,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// Open returns a reader for a blob. The returned reader seeks, so the Control
// API can serve range requests over it.
//
// MimeType is sniffed rather than stored: the same bytes are the same blob no
// matter what content type they arrived with, so a declared type belongs on the
// exchange row that references the digest, not on the blob.
func (s *Store) Open(ctx context.Context, digest string) (io.ReadSeekCloser, core.BlobInfo, error) {
	if !ValidDigest(digest) {
		return nil, core.BlobInfo{}, fmt.Errorf("%w: %q is not a sha256 hex digest", core.ErrBadRequest, digest)
	}
	f, err := os.Open(s.path(digest))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, core.BlobInfo{}, fmt.Errorf("%w: artifact %s", core.ErrNotFound, digest)
		}
		return nil, core.BlobInfo{}, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, core.BlobInfo{}, err
	}

	var head [512]byte
	n, _ := io.ReadFull(f, head[:])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, core.BlobInfo{}, err
	}

	return f, core.BlobInfo{
		Digest:    digest,
		Size:      st.Size(),
		MimeType:  http.DetectContentType(head[:n]),
		CreatedAt: st.ModTime().UTC(),
	}, nil
}

// Stat reports a blob's metadata without opening it for reading.
func (s *Store) Stat(ctx context.Context, digest string) (core.BlobInfo, error) {
	if !ValidDigest(digest) {
		return core.BlobInfo{}, fmt.Errorf("%w: %q is not a sha256 hex digest", core.ErrBadRequest, digest)
	}
	st, err := os.Stat(s.path(digest))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return core.BlobInfo{}, fmt.Errorf("%w: artifact %s", core.ErrNotFound, digest)
		}
		return core.BlobInfo{}, err
	}
	return core.BlobInfo{
		Digest:    digest,
		Size:      st.Size(),
		CreatedAt: st.ModTime().UTC(),
	}, nil
}

// Has reports whether the hub already holds these bytes. Agents call this
// through the artifact endpoint to skip uploads.
func (s *Store) Has(ctx context.Context, digest string) bool {
	if !ValidDigest(digest) {
		return false
	}
	_, err := os.Stat(s.path(digest))
	return err == nil
}

// Delete removes a blob. Deleting a blob that is not there is not an error:
// retention and refcount GC race by nature, and both want the same end state.
func (s *Store) Delete(ctx context.Context, digest string) error {
	if !ValidDigest(digest) {
		return fmt.Errorf("%w: %q is not a sha256 hex digest", core.ErrBadRequest, digest)
	}
	err := os.Remove(s.path(digest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type blobEntry struct {
	digest string
	path   string
	size   int64
	mtime  time.Time
}

// Sweep enforces retention: first drop anything older than maxAge, then drop
// oldest-first until the store is under maxBytes. keep, when non-nil, vetoes
// deletion of a digest that is still referenced.
//
// Age before size, because "too old to matter" is a policy statement and "too
// big" is a disk fact; applying the policy first means the size pass only ever
// deletes things someone might still have wanted, and only when it must.
func (s *Store) Sweep(ctx context.Context, maxAge time.Duration, maxBytes int64, keep func(digest string) bool) (int, error) {
	entries, total, err := s.list()
	if err != nil {
		return 0, err
	}

	removed := 0
	now := time.Now()

	remaining := entries[:0]
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if maxAge > 0 && now.Sub(e.mtime) > maxAge && (keep == nil || !keep(e.digest)) {
			if err := os.Remove(e.path); err == nil {
				removed++
				total -= e.size
				continue
			}
		}
		remaining = append(remaining, e)
	}

	if maxBytes <= 0 || total <= maxBytes {
		return removed, nil
	}

	sort.Slice(remaining, func(i, j int) bool { return remaining[i].mtime.Before(remaining[j].mtime) })
	for _, e := range remaining {
		if total <= maxBytes {
			break
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if keep != nil && keep(e.digest) {
			continue
		}
		if err := os.Remove(e.path); err == nil {
			removed++
			total -= e.size
		}
	}
	return removed, nil
}

// Size reports the total bytes held, for the console's storage panel.
func (s *Store) Size() (int64, int, error) {
	entries, total, err := s.list()
	return total, len(entries), err
}

func (s *Store) list() ([]blobEntry, int64, error) {
	var entries []blobEntry
	var total int64

	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".tmp" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !ValidDigest(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// Deleted underneath the walk by a concurrent sweep. Not an error.
			return nil
		}
		entries = append(entries, blobEntry{digest: name, path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	return entries, total, nil
}

// Digest is a convenience for callers that have the bytes in hand.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NormalizeDigest lowercases and strips a "sha256:" prefix, so callers may use
// either spelling.
func NormalizeDigest(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimPrefix(s, "sha256:")
}
