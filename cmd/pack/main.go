// Command pack builds the extension bundles.
//
// It exists because the extensions have no build step and no node_modules --
// there is no Node toolchain in this project by design -- but they still need
// two things done to them: the shared agent core has to be copied in beside
// each host adapter, and the result has to be zipped.
//
// It also does the one automated check the JavaScript ever gets: every file a
// manifest references must exist in the assembled tree. A typo in a manifest
// path is otherwise a silent failure that only shows up as an extension that
// loads and does nothing.
package main

import (
	"archive/zip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// target is one extension bundle.
type target struct {
	Name   string // chrome
	Source string // extension/chrome
	Ext    string // .zip, or .xpi for Firefox
}

var targets = []target{
	{Name: "chrome", Source: "extension/chrome", Ext: ".zip"},
	// Firefox add-ons are zips with a different extension. The Android build is
	// this same file: Firefox for Android installs the identical add-on.
	{Name: "firefox", Source: "extension/firefox", Ext: ".xpi"},
}

// sharedDir is copied into every bundle as "shared/".
const sharedDir = "extension/shared"

// zipEpoch is a fixed timestamp for every zip entry.
//
// Without it, two builds of identical source produce different bytes, which
// makes an extension zip impossible to verify and makes CI artifacts churn for
// no reason.
var zipEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func main() {
	root := flag.String("root", ".", "repository root")
	out := flag.String("out", "dist", "output directory")
	flag.Parse()

	if err := run(*root, *out); err != nil {
		fmt.Fprintln(os.Stderr, "pack:", err)
		os.Exit(1)
	}
}

func run(root, out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	for _, t := range targets {
		staged := filepath.Join(out, t.Name)
		if err := build(root, staged, t); err != nil {
			return fmt.Errorf("%s: %w", t.Name, err)
		}

		archive := filepath.Join(out, t.Name+t.Ext)
		n, err := zipDir(staged, archive)
		if err != nil {
			return fmt.Errorf("%s: %w", t.Name, err)
		}
		fmt.Printf("%-8s %s (%d files)\n", t.Name, archive, n)
		fmt.Printf("%-8s %s (unpacked, load this one for development)\n", "", staged)
	}
	return nil
}

// build stages one bundle: the host adapter plus the shared core.
func build(root, staged string, t target) error {
	if err := os.RemoveAll(staged); err != nil {
		return err
	}
	if err := copyTree(filepath.Join(root, t.Source), staged); err != nil {
		return fmt.Errorf("copying %s: %w", t.Source, err)
	}
	if err := copyTree(filepath.Join(root, sharedDir), filepath.Join(staged, "shared")); err != nil {
		return fmt.Errorf("copying %s: %w", sharedDir, err)
	}
	return checkManifest(staged)
}

// checkManifest verifies every file the manifest names exists.
//
// This is the whole reason pack is more than a zip command. A manifest that
// points at a file which is not there produces an extension that loads,
// registers nothing, and reports no error anywhere a person will look.
func checkManifest(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}

	var manifest any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("manifest is not valid JSON: %w", err)
	}

	var missing []string
	for _, ref := range collectPaths(manifest) {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(ref))); err != nil {
			missing = append(missing, ref)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("manifest references files that do not exist: %s", strings.Join(missing, ", "))
	}
	return nil
}

// fileExtensions are the ones a manifest can legitimately point at.
var fileExtensions = map[string]bool{
	".js": true, ".html": true, ".css": true, ".json": true,
	".png": true, ".svg": true, ".woff2": true,
}

// collectPaths walks decoded JSON and returns anything that looks like a
// relative file reference. It deliberately ignores URLs and match patterns --
// "<all_urls>" and "https://example.com/*" are not files.
func collectPaths(node any) []string {
	var found []string

	switch v := node.(type) {
	case string:
		if looksLikePath(v) {
			found = append(found, v)
		}
	case []any:
		for _, item := range v {
			found = append(found, collectPaths(item)...)
		}
	case map[string]any:
		// Sorted, so a failure message lists the same paths in the same order
		// on every machine.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			found = append(found, collectPaths(v[k])...)
		}
	}
	return found
}

func looksLikePath(s string) bool {
	if s == "" || strings.ContainsAny(s, "<>*") || strings.Contains(s, "://") {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return false
	}
	return fileExtensions[strings.ToLower(path.Ext(s))]
}

// copyTree copies a directory recursively.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// Editor leftovers have no business in a published extension.
		if strings.HasPrefix(d.Name(), ".") || strings.HasSuffix(d.Name(), "~") {
			return nil
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// zipDir archives a staged bundle, deterministically.
func zipDir(dir, archive string) (int, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return 0, err
	}
	// Sorted entries, fixed timestamps: identical input produces identical
	// bytes, so a published zip can be checked against a rebuild.
	sort.Strings(files)

	f, err := os.Create(archive)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for _, p := range files {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return 0, err
		}
		header := &zip.FileHeader{
			Name:     filepath.ToSlash(rel),
			Method:   zip.Deflate,
			Modified: zipEpoch,
		}
		w, err := zw.CreateHeader(header)
		if err != nil {
			return 0, err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return 0, err
		}
		if _, err := w.Write(body); err != nil {
			return 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	return len(files), f.Close()
}
