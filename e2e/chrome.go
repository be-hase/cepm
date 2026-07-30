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
	"testing"
	"time"
)

const versionsURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// ensureChrome returns a Chrome for Testing binary: $CEPM_E2E_CHROME if set,
// a cached download, or a fresh download into the user cache dir.
// Must be called before the test overrides $HOME.
func ensureChrome(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("CEPM_E2E_CHROME"); bin != "" {
		return bin
	}
	platform, binRel, err := cftPlatform()
	if err != nil {
		t.Skipf("chrome for testing unavailable: %v", err)
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(cacheRoot, "cepm-e2e")

	// Reuse any cached version.
	if entries, err := os.ReadDir(cacheDir); err == nil {
		for _, e := range entries {
			bin := filepath.Join(cacheDir, e.Name(), binRel)
			if _, err := os.Stat(bin); err == nil {
				return bin
			}
		}
	}

	version, url, err := lookupDownload(platform)
	if err != nil {
		t.Skipf("cannot resolve Chrome for Testing download (offline?): %v", err)
	}
	t.Logf("downloading Chrome for Testing %s (%s) ...", version, platform)
	dest := filepath.Join(cacheDir, version)
	if err := downloadAndUnzip(url, dest); err != nil {
		t.Fatalf("download Chrome for Testing: %v", err)
	}
	bin := filepath.Join(dest, binRel)
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("downloaded Chrome missing expected binary %s", bin)
	}
	return bin
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

func lookupDownload(platform string) (version, url string, err error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(versionsURL)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
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
		if d.Platform == platform {
			return payload.Channels.Stable.Version, d.URL, nil
		}
	}
	return "", "", fmt.Errorf("platform %s not in CfT downloads", platform)
}

func downloadAndUnzip(url, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "cft-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	out, err := exec.Command("unzip", "-q", "-o", tmp.Name(), "-d", dest).CombinedOutput()
	if err != nil {
		return fmt.Errorf("unzip: %v\n%s", err, out)
	}
	return nil
}
