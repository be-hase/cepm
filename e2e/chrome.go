//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const versionsURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// chromeFetcher resolves a Chrome for Testing binary, caching per version.
// The fields are what the unit tests substitute; production uses the real
// endpoint and the user cache dir.
//
// Chrome is deliberately *not* pinned: driving whatever Chrome currently
// ships is the early warning this suite exists to give. What is pinned is
// the integrity of the cache — the version actually asked for, and only
// completely-extracted trees.
//
// Trust model: two Google-operated HTTPS origins, the metadata on
// googlechromelabs.github.io and the archive on storage.googleapis.com,
// with TLS as the boundary. Chrome for Testing publishes no per-artifact
// digest to check the download against.
type chromeFetcher struct {
	versionsURL string
	cacheDir    string
	platform    string
	binRel      string // binary path inside the extracted archive
	// beforeQuarantine runs once this fetcher has found the destination
	// occupied by an unusable tree and is about to move it aside — the
	// exact point two repairs of the same entry collide. Test-only seam
	// (nil in production), per fetcher rather than global.
	beforeQuarantine func()
}

// chromeReporter is the slice of *testing.T resolveChrome needs. Splitting
// it out is what lets a test prove the skip-versus-fail decision: skipping a
// broken fetch would leave CI green with the real-Chrome scenarios never
// executed, and that regression has to be catchable without a real download.
type chromeReporter interface {
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// ensureChrome returns a Chrome for Testing binary: $CEPM_E2E_CHROME if set,
// the cached copy of the current stable version, or a fresh download.
// Must be called before the test overrides $HOME.
func ensureChrome(t *testing.T) string {
	t.Helper()
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return resolveChrome(t, filepath.Join(cacheRoot, "cepm-e2e"), versionsURL)
}

// resolveChrome is ensureChrome's decision-making, over an interface a test
// can substitute. Only an unsupported platform skips; every other failure is
// fatal.
func resolveChrome(r chromeReporter, cacheDir, metadataURL string) string {
	if bin := os.Getenv("CEPM_E2E_CHROME"); bin != "" {
		// Prove it runs: a stale path in the environment would otherwise
		// surface much later as an unexplained scenario failure.
		out, err := exec.Command(bin, "--version").CombinedOutput()
		if err != nil {
			r.Fatalf("CEPM_E2E_CHROME=%s is not usable: %v\n%s", bin, err, out)
			return ""
		}
		r.Logf("using Chrome from CEPM_E2E_CHROME: %s (%s)", bin, strings.TrimSpace(string(out)))
		return bin
	}
	// The only skip: no Chrome for Testing exists for this platform, so
	// there is nothing to test rather than something broken.
	platform, binRel, err := cftPlatform()
	if err != nil {
		r.Skipf("chrome for testing unavailable: %v", err)
		return ""
	}
	f := &chromeFetcher{
		versionsURL: metadataURL,
		cacheDir:    cacheDir,
		platform:    platform,
		binRel:      binRel,
	}
	bin, version, err := f.ensure(r.Logf)
	if err != nil {
		r.Fatalf("cannot obtain Chrome for Testing: %v", err)
		return ""
	}
	r.Logf("using Chrome for Testing %s (%s): %s", version, platform, bin)
	return bin
}

// ensure resolves the current stable version first and reuses the cache only
// for *that* version: reusing whatever happened to be cached made a stale
// Chrome permanent locally, which defeats tracking the current one.
func (f *chromeFetcher) ensure(logf func(string, ...any)) (bin, version string, err error) {
	version, url, err := f.lookupDownload()
	if err != nil {
		// Offline with an intact cache: any cached version beats nothing,
		// but say which one, since it is not necessarily current.
		if cached, cachedVersion := f.newestCached(); cached != "" {
			logf("Chrome metadata unreachable (%v); falling back to cached %s", err, cachedVersion)
			return cached, cachedVersion, nil
		}
		return "", "", err
	}
	dest := filepath.Join(f.cacheDir, version)
	if b := filepath.Join(dest, f.binRel); fileExists(b) {
		return b, version, nil
	}
	logf("downloading Chrome for Testing %s (%s) ...", version, f.platform)
	if err := f.download(url, dest); err != nil {
		return "", "", err
	}
	b := filepath.Join(dest, f.binRel)
	if !fileExists(b) {
		return "", "", fmt.Errorf("downloaded Chrome missing expected binary %s", b)
	}
	return b, version, nil
}

// newestCached returns the newest usable cached binary. Chrome versions are
// four numeric components, so they must be compared numerically: by string
// order "151.0.7922.9" would beat "151.0.7922.71".
func (f *chromeFetcher) newestCached() (bin, version string) {
	entries, err := os.ReadDir(f.cacheDir)
	if err != nil {
		return "", ""
	}
	for _, e := range entries {
		b := filepath.Join(f.cacheDir, e.Name(), f.binRel)
		if !fileExists(b) {
			continue
		}
		if version == "" || compareChromeVersions(e.Name(), version) > 0 {
			bin, version = b, e.Name()
		}
	}
	return bin, version
}

// compareChromeVersions orders dotted numeric versions, treating a
// non-numeric component as lowest so a stray directory never wins.
func compareChromeVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var an, bn int
		if i < len(as) {
			an, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bn, _ = strconv.Atoi(bs[i])
		}
		if an != bn {
			if an > bn {
				return 1
			}
			return -1
		}
	}
	return 0
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// cftPlatform maps GOOS/GOARCH to the CfT platform key and the binary path
// inside the archive.
func cftPlatform() (platform, binRel string, err error) {
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "mac-arm64", filepath.Join("chrome-mac-arm64",
			"Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"), nil
	case runtime.GOOS == "darwin":
		return "mac-x64", filepath.Join("chrome-mac-x64",
			"Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"), nil
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		return "linux64", filepath.Join("chrome-linux64", "chrome"), nil
	default:
		return "", "", fmt.Errorf("no Chrome for Testing build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

func (f *chromeFetcher) lookupDownload() (version, url string, err error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(f.versionsURL)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GET %s: %s", f.versionsURL, resp.Status)
	}
	var payload struct {
		Channels struct {
			Stable struct {
				Version   string `json:"version"`
				Downloads struct {
					Chrome []struct {
						Platform string `json:"platform"`
						URL      string `json:"url"`
					} `json:"chrome"`
				} `json:"downloads"`
			} `json:"Stable"`
		} `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", err
	}
	for _, d := range payload.Channels.Stable.Downloads.Chrome {
		if d.Platform == f.platform {
			if payload.Channels.Stable.Version == "" {
				return "", "", fmt.Errorf("CfT metadata has no stable version")
			}
			return payload.Channels.Stable.Version, d.URL, nil
		}
	}
	return "", "", fmt.Errorf("platform %s not in CfT downloads", f.platform)
}

// download extracts into a staging directory and moves it into place only
// once the expected binary is there: an interrupted unzip used to leave a
// partial tree that the next run happily accepted as a cache hit.
func (f *chromeFetcher) download(url, dest string) error {
	if err := os.MkdirAll(f.cacheDir, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(f.cacheDir, ".staging-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	tmp, err := os.CreateTemp(staging, "cft-*.zip")
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	tree := filepath.Join(staging, "tree")
	if out, err := exec.Command("unzip", "-q", "-o", tmp.Name(), "-d", tree).CombinedOutput(); err != nil {
		return fmt.Errorf("unzip: %v\n%s", err, out)
	}
	if !fileExists(filepath.Join(tree, f.binRel)) {
		return fmt.Errorf("archive from %s has no %s", url, f.binRel)
	}
	// Another run may have finished the same version first; its tree is as
	// good as ours, so treat losing the race as success. A *partial* tree
	// at dest (an older cepm interrupted mid-unzip) has no such claim: move
	// it aside so this complete one can take its place, rather than failing
	// every future run against debris nothing repairs.
	if err := os.Rename(tree, dest); err != nil {
		if fileExists(filepath.Join(dest, f.binRel)) {
			return nil
		}
		if f.beforeQuarantine != nil {
			f.beforeQuarantine()
		}
		quarantine, qerr := os.MkdirTemp(f.cacheDir, ".broken-*")
		if qerr != nil {
			return err
		}
		defer os.RemoveAll(quarantine)
		if mErr := os.Rename(dest, filepath.Join(quarantine, "old")); mErr != nil && !os.IsNotExist(mErr) {
			return err
		}
		if rErr := os.Rename(tree, dest); rErr != nil {
			// Same race as above, one step later: another run may have
			// repaired the entry while we were quarantining. A complete
			// tree at dest is the outcome we wanted either way.
			if fileExists(filepath.Join(dest, f.binRel)) {
				return nil
			}
			return rErr
		}
		return nil
	}
	return nil
}
