// Package scan discovers Chrome extension directories inside a repository and
// reads the repository-side cepm.toml configuration.
package scan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	toml "github.com/pelletier/go-toml/v2"
)

// MaxAutoDetect is the number of auto-detected extensions above which we ask
// repo authors to declare them explicitly in cepm.toml.
const MaxAutoDetect = 20

// Extension is a detected extension directory.
type Extension struct {
	Dir  string // repo-relative, "." for repo root
	Name string // from manifest.json
}

// RepoConfig is the optional cepm.toml a repository author can commit at the
// repo root.
type RepoConfig struct {
	Extensions []string `toml:"extensions"`
	Track      string   `toml:"track"`       // "branch" | "tag"
	TagPattern string   `toml:"tag_pattern"` // glob, e.g. "v*"
	Prerelease bool     `toml:"prerelease"`  // follow prerelease versions too
}

// LoadRepoConfig reads <repoDir>/cepm.toml. It returns (nil, nil) when the
// file does not exist.
func LoadRepoConfig(repoDir string) (*RepoConfig, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, "cepm.toml"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg RepoConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse cepm.toml: %w", err)
	}
	if cfg.Track != "" && cfg.Track != "branch" && cfg.Track != "tag" {
		return nil, fmt.Errorf(`cepm.toml: track must be "branch" or "tag", got %q`, cfg.Track)
	}
	if cfg.TagPattern != "" && !ValidTagPattern(cfg.TagPattern) {
		return nil, fmt.Errorf("cepm.toml: invalid tag_pattern %q", cfg.TagPattern)
	}
	return &cfg, nil
}

var tagPatternRe = regexp.MustCompile(`^[A-Za-z0-9._*?\[\]{}/@+-]+$`)

// ValidTagPattern reports whether a glob is safe to hand to git. Without this
// a repository could set tag_pattern to something starting with "-" and have
// git read it as an option (e.g. --format=...), taking over which revision
// cepm follows.
func ValidTagPattern(p string) bool {
	return !strings.HasPrefix(p, "-") && tagPatternRe.MatchString(p)
}

// Detect finds extension directories in repoDir. If cepm.toml lists
// extensions explicitly, only those are used (and validated); otherwise the
// tree is walked looking for manifest.json files.
func Detect(repoDir string) ([]Extension, error) {
	cfg, err := LoadRepoConfig(repoDir)
	if err != nil {
		return nil, err
	}
	if cfg != nil && len(cfg.Extensions) > 0 {
		return fromConfig(repoDir, cfg.Extensions)
	}
	return walk(repoDir)
}

func fromConfig(repoDir string, dirs []string) ([]Extension, error) {
	var exts []Extension
	seen := map[string]bool{}
	for _, dir := range dirs {
		rel := filepath.Clean(dir)
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("cepm.toml: extension dir %q must be inside the repository", dir)
		}
		if seen[rel] {
			return nil, fmt.Errorf("cepm.toml: extension dir %q is listed twice", dir)
		}
		seen[rel] = true
		abs := filepath.Join(repoDir, rel)
		// Clean() does not follow symlinks, so re-check the resolved path:
		// a symlink in the repo could otherwise point the "extension" at any
		// directory on the machine.
		if err := ensureInside(repoDir, abs); err != nil {
			return nil, fmt.Errorf("cepm.toml: extension dir %q: %w", dir, err)
		}
		name, err := manifestName(abs)
		if err != nil {
			return nil, fmt.Errorf("cepm.toml: extension dir %q: %w", dir, err)
		}
		exts = append(exts, Extension{Dir: rel, Name: name})
	}
	return exts, nil
}

// ensureInside verifies that path, with symlinks resolved, stays under root.
func ensureInside(root, path string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("resolves to %s, which is outside the repository", realPath)
	}
	return nil
}

func walk(repoDir string) ([]Extension, error) {
	var exts []Extension
	err := filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		base := d.Name()
		if path != repoDir && (base == "node_modules" || strings.HasPrefix(base, ".")) {
			return filepath.SkipDir
		}
		name, err := manifestName(path)
		if err != nil {
			if errors.Is(err, errBadManifest) {
				// A manifest.json that exists but does not parse means the
				// tree is in a bad state (mid-conflict, partial checkout).
				// Treating it as "not an extension" would unregister a
				// working extension, so refuse to scan instead.
				rel, _ := filepath.Rel(repoDir, path)
				return fmt.Errorf("%s: %w", rel, err)
			}
			return nil // no manifest here; keep walking
		}
		rel, relErr := filepath.Rel(repoDir, path)
		if relErr != nil {
			return relErr
		}
		exts = append(exts, Extension{Dir: rel, Name: name})
		// An extension does not contain another extension; don't descend.
		if path != repoDir {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(exts, func(i, j int) bool { return exts[i].Dir < exts[j].Dir })
	return exts, nil
}

// ManifestVersion returns the "version" declared in <dir>/manifest.json.
// Comparing it with what Chrome reports reveals manifest changes that a
// setEnabled-based reload could not apply (Chrome keeps the manifest it
// cached at install time until a full reload or restart).
func ManifestVersion(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "", err
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", err
	}
	return m.Version, nil
}

// errBadManifest marks a manifest.json that exists but cannot be used, as
// opposed to a directory that simply has none.
var errBadManifest = errors.New("manifest.json is present but unusable")

// manifestName parses <dir>/manifest.json and returns the extension name.
// A missing file yields a plain error; a present-but-broken one wraps
// errBadManifest so callers can tell the two apart.
func manifestName(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "", fmt.Errorf("manifest.json: %w", err)
	}
	var m struct {
		ManifestVersion int    `json:"manifest_version"`
		Name            string `json:"name"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("%w: %v", errBadManifest, err)
	}
	if m.ManifestVersion != 2 && m.ManifestVersion != 3 {
		// Not an extension manifest (some other tool's manifest.json).
		return "", fmt.Errorf("manifest.json: unsupported manifest_version %d", m.ManifestVersion)
	}
	if m.Name == "" {
		return "", fmt.Errorf("%w: missing name", errBadManifest)
	}
	// i18n placeholder names are resolved from _locales at runtime; fall back
	// to the directory name for display purposes.
	if strings.HasPrefix(m.Name, "__MSG_") {
		return filepath.Base(dir), nil
	}
	return sanitizeName(m.Name), nil
}

// sanitizeName strips what a repository must not be able to put on a user's
// terminal. Names are printed in tables and in the numbered menus people
// answer, so an embedded newline or escape sequence could forge a row and
// make someone enable a different extension than the one they read.
func sanitizeName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || r == '‎' || r == '‏' || r == '‮' {
			return -1
		}
		return r
	}, name)
	cleaned = strings.TrimSpace(cleaned)
	// Chrome itself caps the manifest name at 45 characters.
	if len([]rune(cleaned)) > 45 {
		cleaned = string([]rune(cleaned)[:45])
	}
	if cleaned == "" {
		return "(unnamed)"
	}
	return cleaned
}
