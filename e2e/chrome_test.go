//go:build e2e

package e2e

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chromeServer serves CfT metadata and a zip whose contents the test picks,
// so the cache logic can be exercised without the network.
type chromeServer struct {
	*httptest.Server
	version   string
	zipEntry  string // path inside the archive ("" = empty archive)
	downloads int
}

func newChromeServer(t *testing.T, version, zipEntry string) *chromeServer {
	t.Helper()
	cs := &chromeServer{version: version, zipEntry: zipEntry}
	mux := http.NewServeMux()
	mux.HandleFunc("/versions.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"channels":{"Stable":{"version":%q,"downloads":{"chrome":[
			{"platform":"testplat","url":%q}]}}}}`, cs.version, cs.URL+"/chrome.zip")
	})
	mux.HandleFunc("/chrome.zip", func(w http.ResponseWriter, r *http.Request) {
		cs.downloads++
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		if cs.zipEntry != "" {
			f, err := zw.Create(cs.zipEntry)
			if err != nil {
				t.Error(err)
			} else {
				fmt.Fprintf(f, "#!/bin/sh\necho chrome %s\n", cs.version)
			}
		}
		if err := zw.Close(); err != nil {
			t.Error(err)
		}
		w.Write(buf.Bytes())
	})
	cs.Server = httptest.NewServer(mux)
	t.Cleanup(cs.Close)
	return cs
}

func newFetcher(t *testing.T, cs *chromeServer) *chromeFetcher {
	t.Helper()
	return &chromeFetcher{
		versionsURL: cs.URL + "/versions.json",
		cacheDir:    t.TempDir(),
		platform:    "testplat",
		binRel:      filepath.Join("chrome-testplat", "chrome"),
	}
}

func quietf(string, ...any) {}

// The cache must follow the metadata, not the other way round: a stale
// version sitting in the cache used to be reused forever, which defeats the
// point of testing against whatever Chrome currently ships.
func TestChromeFetcherPrefersTheCurrentVersion(t *testing.T) {
	cs := newChromeServer(t, "100.0.0", filepath.Join("chrome-testplat", "chrome"))
	f := newFetcher(t, cs)

	// A stale cache entry that looks complete.
	stale := filepath.Join(f.cacheDir, "99.0.0", "chrome-testplat")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "chrome"), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	bin, version, err := f.ensure(quietf)
	if err != nil {
		t.Fatal(err)
	}
	if version != "100.0.0" {
		t.Errorf("version = %q, want the metadata's 100.0.0", version)
	}
	if !strings.Contains(bin, "100.0.0") {
		t.Errorf("binary %q is not from the current version", bin)
	}

	// And the freshly cached copy is reused without downloading again.
	before := cs.downloads
	if _, _, err := f.ensure(quietf); err != nil {
		t.Fatal(err)
	}
	if cs.downloads != before {
		t.Errorf("a complete cache entry should be reused, got %d extra download(s)", cs.downloads-before)
	}
}

// A download whose archive lacks the expected binary must leave no cache
// entry behind: the staging tree is discarded, so the next run retries
// instead of treating the debris as a hit.
func TestChromeFetcherLeavesNoCacheAfterABadArchive(t *testing.T) {
	cs := newChromeServer(t, "100.0.0", "chrome-testplat/not-the-binary")
	f := newFetcher(t, cs)

	if _, _, err := f.ensure(quietf); err == nil {
		t.Fatal("an archive without the expected binary must fail")
	}
	entries, err := os.ReadDir(f.cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".staging-") {
			t.Errorf("a failed download left a cache entry: %s", e.Name())
		}
	}

	// The next run, with a good archive, succeeds — the failure was not
	// cached as a version.
	cs.zipEntry = filepath.Join("chrome-testplat", "chrome")
	if _, version, err := f.ensure(quietf); err != nil || version != "100.0.0" {
		t.Fatalf("retry after a bad archive: version=%q err=%v", version, err)
	}
}

// Offline with a complete cache is still usable — the suite would otherwise
// be unrunnable on a plane — but the fallback is explicit, not silent.
func TestChromeFetcherFallsBackToCacheWhenOffline(t *testing.T) {
	cs := newChromeServer(t, "100.0.0", filepath.Join("chrome-testplat", "chrome"))
	f := newFetcher(t, cs)
	if _, _, err := f.ensure(quietf); err != nil {
		t.Fatal(err)
	}
	cs.Close() // metadata unreachable from here on

	var logged strings.Builder
	bin, version, err := f.ensure(func(format string, args ...any) {
		fmt.Fprintf(&logged, format, args...)
	})
	if err != nil {
		t.Fatalf("a complete cache should still be usable offline: %v", err)
	}
	if version != "100.0.0" || !fileExists(bin) {
		t.Errorf("unexpected offline result: version=%q bin=%q", version, bin)
	}
	if !strings.Contains(logged.String(), "falling back to cached") {
		t.Errorf("the offline fallback must be stated, got log %q", logged.String())
	}

	// With no usable cache at all, offline is a clean failure.
	empty := &chromeFetcher{versionsURL: f.versionsURL, cacheDir: t.TempDir(),
		platform: f.platform, binRel: f.binRel}
	if _, _, err := empty.ensure(quietf); err == nil {
		t.Error("offline with an empty cache must fail")
	}
}
