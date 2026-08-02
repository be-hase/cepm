//go:build e2e

package e2e

import (
	"archive/zip"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// chromeServer serves CfT metadata and a zip whose contents the test picks,
// so the cache logic can be exercised without the network.
type chromeServer struct {
	*httptest.Server
	mu        sync.Mutex
	version   string
	zipEntry  string // path inside the archive ("" = empty archive)
	downloads int
}

func (cs *chromeServer) downloadCount() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.downloads
}

func (cs *chromeServer) setZipEntry(entry string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.zipEntry = entry
}

func newChromeServer(t *testing.T, version, zipEntry string) *chromeServer {
	t.Helper()
	cs := &chromeServer{version: version, zipEntry: zipEntry}
	mux := http.NewServeMux()
	mux.HandleFunc("/versions.json", func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		version := cs.version
		cs.mu.Unlock()
		fmt.Fprintf(w, `{"channels":{"Stable":{"version":%q,"downloads":{"chrome":[
			{"platform":"testplat","url":%q}]}}}}`, version, cs.URL+"/chrome.zip")
	})
	mux.HandleFunc("/chrome.zip", func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		cs.downloads++
		entry, version := cs.zipEntry, cs.version
		cs.mu.Unlock()
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		if entry != "" {
			f, err := zw.Create(entry)
			if err != nil {
				t.Error(err)
			} else {
				fmt.Fprintf(f, "#!/bin/sh\necho chrome %s\n", version)
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
	before := cs.downloadCount()
	if _, _, err := f.ensure(quietf); err != nil {
		t.Fatal(err)
	}
	if got := cs.downloadCount(); got != before {
		t.Errorf("a complete cache entry should be reused, got %d extra download(s)", got-before)
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
	cs.setZipEntry(filepath.Join("chrome-testplat", "chrome"))
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

// An incomplete cache directory for the *current* version is debris from an
// interrupted older run; the fetcher must be able to replace it, or every
// future run fails against something nothing repairs.
func TestChromeFetcherRepairsAnIncompleteCurrentVersion(t *testing.T) {
	cs := newChromeServer(t, "100.0.0", filepath.Join("chrome-testplat", "chrome"))
	f := newFetcher(t, cs)

	// A directory named like the current version, but with no binary.
	partial := filepath.Join(f.cacheDir, "100.0.0", "chrome-testplat")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "half-extracted"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	bin, version, err := f.ensure(quietf)
	if err != nil {
		t.Fatalf("an incomplete current-version cache must be repaired: %v", err)
	}
	if version != "100.0.0" || !fileExists(bin) {
		t.Errorf("unexpected result: version=%q bin=%q", version, bin)
	}
}

// Chrome versions are four numeric components: "…7922.9" is older than
// "…7922.71", which string ordering gets backwards.
func TestOfflineFallbackPicksTheNumericallyNewestCache(t *testing.T) {
	cs := newChromeServer(t, "151.0.7922.71", filepath.Join("chrome-testplat", "chrome"))
	f := newFetcher(t, cs)
	for _, v := range []string{"151.0.7922.9", "151.0.7922.71"} {
		dir := filepath.Join(f.cacheDir, v, "chrome-testplat")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "chrome"), []byte(v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cs.Close() // offline

	_, version, err := f.ensure(quietf)
	if err != nil {
		t.Fatal(err)
	}
	if version != "151.0.7922.71" {
		t.Errorf("offline fallback chose %q, want the numerically newest 151.0.7922.71", version)
	}
}

// Every fetch failure must be a failure, not a skip: skipping made CI green
// while the real-Chrome scenarios never ran. ensure returning an error is
// what ensureChrome turns into t.Fatalf.
func TestChromeFetcherReportsFailuresAsErrors(t *testing.T) {
	t.Run("http error", func(t *testing.T) {
		cs := newChromeServer(t, "100.0.0", filepath.Join("chrome-testplat", "chrome"))
		cs.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ".json") {
				fmt.Fprintf(w, `{"channels":{"Stable":{"version":"100.0.0","downloads":{"chrome":[
					{"platform":"testplat","url":%q}]}}}}`, cs.URL+"/chrome.zip")
				return
			}
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		f := newFetcher(t, cs)
		if _, _, err := f.ensure(quietf); err == nil {
			t.Error("an HTTP 500 on the archive must be an error")
		}
	})

	t.Run("missing binary", func(t *testing.T) {
		cs := newChromeServer(t, "100.0.0", "chrome-testplat/not-the-binary")
		f := newFetcher(t, cs)
		if _, _, err := f.ensure(quietf); err == nil {
			t.Error("an archive without the expected binary must be an error")
		}
	})

	t.Run("offline with no cache", func(t *testing.T) {
		cs := newChromeServer(t, "100.0.0", filepath.Join("chrome-testplat", "chrome"))
		f := newFetcher(t, cs)
		cs.Close()
		if _, _, err := f.ensure(quietf); err == nil {
			t.Error("offline with an empty cache must be an error")
		}
	})
}

// A read deadline can expire in the middle of a CDP frame. The bytes read
// so far must be carried over: dropping them leaves every later read
// starting mid-frame, which turns one timeout into a permanently corrupt
// stream (observed as an entire poll window of failures in CI).
func TestCDPPipeSurvivesATimeoutMidFrame(t *testing.T) {
	chromeRead, ourWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer chromeRead.Close()
	defer ourWrite.Close()
	ourRead, chromeWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ourRead.Close()
	defer chromeWrite.Close()
	c := &cdpPipe{w: ourWrite, r: bufio.NewReader(ourRead), rf: ourRead}

	orig := cdpReadTimeoutForTest
	cdpReadTimeoutForTest = 300 * time.Millisecond
	defer func() { cdpReadTimeoutForTest = orig }()

	// A peer that stops mid-frame: the sleep is the stimulus under test (a
	// stalled Chrome), not a synchronization device — the assertions below
	// wait on channels.
	timedOut := make(chan struct{})
	written := make(chan struct{})
	go func() {
		defer close(written)
		io.ReadAll(io.LimitReader(chromeRead, 1)) // consume part of the command
		chromeWrite.Write([]byte(`{"id":1,"result":{"half`))
		<-timedOut
		chromeWrite.Write(append([]byte(`":"there"}}`), 0))
		chromeWrite.Write(append([]byte(`{"id":2,"result":{"ok":true}}`), 0))
	}()

	if _, err := c.call("First", nil); err == nil {
		t.Fatal("the interrupted call should time out")
	}
	// The bytes read before the deadline must be held, not dropped: the
	// recovery below can otherwise succeed by luck (a discarded prefix that
	// happens to end at a delimiter parses as garbage and is skipped).
	if len(c.partial) == 0 {
		t.Error("the interrupted frame's bytes were dropped")
	}
	close(timedOut)
	<-written

	// The second call must still parse: if the partial frame had been
	// dropped, the reader would now be mid-JSON and never recover.
	cdpReadTimeoutForTest = 10 * time.Second
	res, err := c.call("Second", nil)
	if err != nil {
		t.Fatalf("the stream did not survive the timeout: %v", err)
	}
	if !strings.Contains(string(res), `"ok":true`) {
		t.Errorf("unexpected result after recovery: %s", res)
	}
}

// fakeReporter records which of the three outcomes resolveChrome chose.
type fakeReporter struct {
	skipped, fatal string
	logs           strings.Builder
}

func (r *fakeReporter) Skipf(format string, args ...any) {
	r.skipped = fmt.Sprintf(format, args...)
}
func (r *fakeReporter) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
}
func (r *fakeReporter) Logf(format string, args ...any) {
	fmt.Fprintf(&r.logs, format, args...)
}

// A broken fetch has to fail the run, not skip it: skipping leaves CI green
// with the real-Chrome scenarios never executed, which is the one regression
// nothing else would catch.
func TestResolveChromeFailsRatherThanSkips(t *testing.T) {
	t.Setenv("CEPM_E2E_CHROME", "")
	if _, _, err := cftPlatform(); err != nil {
		t.Skip("no Chrome for Testing on this platform; the skip path is the one under test elsewhere")
	}

	cases := map[string]func(t *testing.T) (cacheDir, metadataURL string){
		"http error": func(t *testing.T) (string, string) {
			cs := newChromeServer(t, "100.0.0", filepath.Join("chrome-testplat", "chrome"))
			cs.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			})
			return t.TempDir(), cs.URL + "/versions.json"
		},
		"bad archive": func(t *testing.T) (string, string) {
			cs := newChromeServer(t, "100.0.0", "wrong/place")
			return t.TempDir(), cs.URL + "/versions.json"
		},
		"offline without cache": func(t *testing.T) (string, string) {
			cs := newChromeServer(t, "100.0.0", filepath.Join("chrome-testplat", "chrome"))
			url := cs.URL + "/versions.json"
			cs.Close()
			return t.TempDir(), url
		},
	}
	for label, setup := range cases {
		t.Run(label, func(t *testing.T) {
			cacheDir, metadataURL := setup(t)
			r := &fakeReporter{}
			resolveChrome(r, cacheDir, metadataURL)
			if r.fatal == "" {
				t.Errorf("a %s must be fatal, got skip=%q", label, r.skipped)
			}
			if r.skipped != "" {
				t.Errorf("a %s must not skip the run: %q", label, r.skipped)
			}
		})
	}
}

// Two runs repairing the same incomplete cache entry must both succeed and
// leave a complete cache: the loser of the race wants exactly what the
// winner produced. Both are gathered at the promotion stage, each holding a
// verified tree, before either is allowed to install it.
func TestConcurrentRepairOfAnIncompleteCacheBothSucceed(t *testing.T) {
	cs := newChromeServer(t, "100.0.0", filepath.Join("chrome-testplat", "chrome"))
	cacheDir := t.TempDir()
	writeIncompleteCache(t, cacheDir)

	const runners = 2
	atPromotion := make(chan struct{}, runners)
	release := make(chan struct{})
	barrier := func() {
		atPromotion <- struct{}{}
		<-release
	}

	type result struct {
		bin string
		err error
	}
	results := make(chan result, runners)
	for i := 0; i < runners; i++ {
		go func() {
			f := newRepairFetcher(cs, cacheDir)
			f.beforePromote = barrier
			bin, _, err := f.ensure(quietf)
			results <- result{bin, err}
		}()
	}
	for i := 0; i < runners; i++ {
		select {
		case <-atPromotion:
		case <-time.After(30 * time.Second):
			t.Fatal("a repair never reached the promotion stage")
		}
	}
	close(release)

	for i := 0; i < runners; i++ {
		select {
		case res := <-results:
			if res.err != nil {
				t.Errorf("a concurrent repair failed: %v", res.err)
			} else if !fileExists(res.bin) {
				t.Errorf("a concurrent repair returned a missing binary: %s", res.bin)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a concurrent repair never finished")
		}
	}
	if !fileExists(filepath.Join(cacheDir, "100.0.0", "chrome-testplat", "chrome")) {
		t.Error("the cache entry should be complete afterwards")
	}
}

// The guarantee behind that: promotion is mutually exclusive. While one run
// holds the promotion lock, another must not be inside it — that overlap is
// what let a run move aside a tree the other had just installed and then
// return a path with no binary in it.
func TestPromotionIsSerialized(t *testing.T) {
	cs := newChromeServer(t, "100.0.0", filepath.Join("chrome-testplat", "chrome"))
	cacheDir := t.TempDir()
	writeIncompleteCache(t, cacheDir)

	firstInside := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		f := newRepairFetcher(cs, cacheDir)
		f.insidePromoteLock = func() {
			close(firstInside)
			<-releaseFirst
		}
		_, _, err := f.ensure(quietf)
		firstDone <- err
	}()
	select {
	case <-firstInside:
	case <-time.After(30 * time.Second):
		t.Fatal("the first repair never entered the promotion lock")
	}

	secondInside := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		f := newRepairFetcher(cs, cacheDir)
		f.insidePromoteLock = func() { close(secondInside) }
		_, _, err := f.ensure(quietf)
		secondDone <- err
	}()

	// It must block: the wait is bounded, and entering within it is the
	// failure — a passing run pays this once.
	select {
	case <-secondInside:
		t.Fatal("two repairs were inside the promotion lock at once")
	case <-time.After(500 * time.Millisecond):
	}

	close(releaseFirst)
	for _, done := range []chan error{firstDone, secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("a serialized repair failed: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a serialized repair never finished")
		}
	}
	if !fileExists(filepath.Join(cacheDir, "100.0.0", "chrome-testplat", "chrome")) {
		t.Error("the cache entry should be complete afterwards")
	}
}

// writeIncompleteCache leaves the debris an interrupted extraction would:
// a directory named after the current version, with no binary in it.
func writeIncompleteCache(t *testing.T, cacheDir string) {
	t.Helper()
	partial := filepath.Join(cacheDir, "100.0.0", "chrome-testplat")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "half"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newRepairFetcher(cs *chromeServer, cacheDir string) *chromeFetcher {
	return &chromeFetcher{
		versionsURL: cs.URL + "/versions.json", cacheDir: cacheDir,
		platform: "testplat", binRel: filepath.Join("chrome-testplat", "chrome"),
	}
}
