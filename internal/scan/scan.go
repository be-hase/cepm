// Package scan discovers Chrome extension directories inside a repository and
// reads the repository-side cepm.toml configuration.
package scan

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	return &cfg, nil
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
	for _, dir := range dirs {
		rel := filepath.Clean(dir)
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("cepm.toml: extension dir %q must be inside the repository", dir)
		}
		name, err := manifestName(filepath.Join(repoDir, rel))
		if err != nil {
			return nil, fmt.Errorf("cepm.toml: extension dir %q: %w", dir, err)
		}
		exts = append(exts, Extension{Dir: rel, Name: name})
	}
	return exts, nil
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
			return nil // not an extension dir; keep walking
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

// manifestName parses <dir>/manifest.json and returns the extension name.
// It returns an error when the file is missing or not a valid extension
// manifest (no manifest_version 2/3, or no name).
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
		return "", fmt.Errorf("manifest.json: %w", err)
	}
	if m.ManifestVersion != 2 && m.ManifestVersion != 3 {
		return "", fmt.Errorf("manifest.json: unsupported manifest_version %d", m.ManifestVersion)
	}
	if m.Name == "" {
		return "", fmt.Errorf("manifest.json: missing name")
	}
	// i18n placeholder names are resolved from _locales at runtime; fall back
	// to the directory name for display purposes.
	if strings.HasPrefix(m.Name, "__MSG_") {
		return filepath.Base(dir), nil
	}
	return m.Name, nil
}
