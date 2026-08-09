package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up to the module root, so these tests run from the package
// directory the way `go test ./...` invokes them.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 5 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root")
	return ""
}

// The real extensions must package. This is the only automated check the
// JavaScript gets, and its most valuable half is checkManifest: a manifest path
// typo produces an extension that loads, registers nothing, and reports no
// error anywhere a person will look.
func TestRealExtensionsPackage(t *testing.T) {
	root := repoRoot(t)
	out := t.TempDir()

	if err := run(root, out); err != nil {
		t.Fatalf("pack: %v", err)
	}

	for _, tg := range targets {
		archive := filepath.Join(out, tg.Name+tg.Ext)
		zr, err := zip.OpenReader(archive)
		if err != nil {
			t.Fatalf("%s: opening %s: %v", tg.Name, archive, err)
		}

		names := map[string]bool{}
		for _, f := range zr.File {
			names[f.Name] = true
		}
		_ = zr.Close()

		for _, want := range []string{
			"manifest.json",
			"shared/protocol.js",
			"shared/channel.js",
			"shared/agent.js",
			"shared/commands.js",
			"shared/domsnap.js",
			"shared/dommirror.js",
			"shared/netlog.js",
			"shared/capabilities.js",
			"background.js",
			"content.js",
			"popup.html",
			"popup.js",
		} {
			if !names[want] {
				t.Errorf("%s bundle is missing %s", tg.Name, want)
			}
		}
	}
}

// The unpacked directory is what a person loads during development, so it has
// to be complete on its own rather than only inside the zip.
func TestUnpackedDirectoryIsComplete(t *testing.T) {
	root := repoRoot(t)
	out := t.TempDir()

	if err := run(root, out); err != nil {
		t.Fatalf("pack: %v", err)
	}

	for _, tg := range targets {
		staged := filepath.Join(out, tg.Name)
		for _, want := range []string{"manifest.json", "shared/protocol.js", "background.js"} {
			if _, err := os.Stat(filepath.Join(staged, filepath.FromSlash(want))); err != nil {
				t.Errorf("%s unpacked bundle is missing %s", tg.Name, want)
			}
		}
	}
}

func TestManifestReferencesAreChecked(t *testing.T) {
	dir := t.TempDir()

	manifest := map[string]any{
		"manifest_version": 3,
		"name":             "test",
		"background":       map[string]any{"service_worker": "background.js"},
		"content_scripts": []any{
			map[string]any{
				"matches": []any{"<all_urls>"},
				"js":      []any{"present.js", "missing.js"},
			},
		},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, name := range []string{"background.js", "present.js"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("// x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	err := checkManifest(dir)
	if err == nil {
		t.Fatal("a manifest referencing a missing file was accepted")
	}
	if !strings.Contains(err.Error(), "missing.js") {
		t.Errorf("error = %v, want it to name missing.js", err)
	}
	if strings.Contains(err.Error(), "present.js") {
		t.Errorf("error = %v, wrongly blames a file that exists", err)
	}
}

func TestManifestCheckPassesWhenComplete(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"manifest_version":3,"background":{"service_worker":"bg.js"},"icons":{"48":"icon.png"}}`)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, name := range []string{"bg.js", "icon.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := checkManifest(dir); err != nil {
		t.Errorf("checkManifest: %v", err)
	}
}

// Match patterns and URLs are not files, and treating them as such would make
// every manifest fail.
func TestLooksLikePath(t *testing.T) {
	yes := []string{"background.js", "shared/protocol.js", "popup.html", "style.css", "icons/48.png"}
	no := []string{
		"<all_urls>",
		"https://example.com/*",
		"*://*/*",
		"/etc/passwd",
		"../../secrets.js",
		"storage",
		"webRequestBlocking",
		"MAIN",
		"document_start",
		"0.1.0",
	}
	for _, s := range yes {
		if !looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = true, want false", s)
		}
	}
}

// Identical input must produce identical bytes, or a published extension zip
// cannot be checked against a rebuild.
func TestZipIsDeterministic(t *testing.T) {
	root := repoRoot(t)

	first, second := t.TempDir(), t.TempDir()
	if err := run(root, first); err != nil {
		t.Fatalf("first pack: %v", err)
	}
	if err := run(root, second); err != nil {
		t.Fatalf("second pack: %v", err)
	}

	for _, tg := range targets {
		a, err := os.ReadFile(filepath.Join(first, tg.Name+tg.Ext))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(second, tg.Name+tg.Ext))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s bundle is not reproducible: two builds of identical source differ", tg.Name)
		}
	}
}

// The Firefox bundle is the Android deliverable, so it has to declare Android
// support and carry the capture implementation that makes it full fidelity.
func TestFirefoxBundleIsTheAndroidBuild(t *testing.T) {
	root := repoRoot(t)
	out := t.TempDir()
	if err := run(root, out); err != nil {
		t.Fatalf("pack: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(out, "firefox", "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var manifest struct {
		BrowserSpecific struct {
			GeckoAndroid map[string]any `json:"gecko_android"`
		} `json:"browser_specific_settings"`
		Background struct {
			Scripts []string `json:"scripts"`
		} `json:"background"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}

	if len(manifest.BrowserSpecific.GeckoAndroid) == 0 {
		t.Error("the Firefox manifest does not declare gecko_android, so Fenix will not install it")
	}
	var hasCapture bool
	for _, s := range manifest.Background.Scripts {
		if strings.Contains(s, "netlog-firefox") {
			hasCapture = true
		}
	}
	if !hasCapture {
		t.Error("the Firefox background does not load its filterResponseData capture")
	}
}
