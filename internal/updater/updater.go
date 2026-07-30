// Package updater pulls managed repositories and determines which extensions
// changed. It is the single code path shared by "cepm update" (CLI) and the
// native host's periodic loop; concurrent runs are serialized with a file
// lock.
package updater

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/mod/semver"

	"github.com/be-hase/cepm/internal/extid"
	"github.com/be-hase/cepm/internal/gitx"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/scan"
	"github.com/be-hase/cepm/internal/state"
)

// ExtChange identifies an extension affected by an update.
type ExtChange struct {
	RepoName string
	Dir      string // repo-relative
	AbsDir   string
	ID       string
	Name     string
}

// RepoResult reports the outcome of updating one repository.
type RepoResult struct {
	Name       string
	OldRef     string // commit or tag before the update
	NewRef     string // commit or tag after the update
	Updated    bool   // the working tree moved to a new revision
	Skipped    bool
	SkipReason string
	Warnings   []string
	Err        error
	Changed    []ExtChange       // existing extensions whose files changed
	Added      []ExtChange       // newly detected extensions (need manual load)
	Removed    []state.Extension // extensions that disappeared from the repo
}

// Options controls update behavior.
type Options struct {
	StashDirty bool // stash+pop around the pull when the tree is dirty
}

// LockTimeout bounds how long we wait for a concurrent update to finish.
const LockTimeout = 5 * time.Minute

// WithLock runs fn while holding the global update lock. All state.json
// writers must go through this.
func WithLock(ctx context.Context, fn func() error) error {
	lockPath, err := paths.UpdateLockPath()
	if err != nil {
		return err
	}
	if err := paths.EnsureLayout(); err != nil {
		return err
	}
	fl := flock.New(lockPath)
	lockCtx, cancel := context.WithTimeout(ctx, LockTimeout)
	defer cancel()
	ok, err := fl.TryLockContext(lockCtx, 200*time.Millisecond)
	if err != nil || !ok {
		return fmt.Errorf("another cepm update is in progress (lock %s): %w", lockPath, err)
	}
	defer fl.Unlock()
	return fn()
}

// Update pulls the named repositories (all registered repos when names is
// empty) and records the results in state.json.
func Update(ctx context.Context, names []string, opts Options) ([]RepoResult, error) {
	var results []RepoResult
	err := WithLock(ctx, func() error {
		st, err := state.Load()
		if err != nil {
			return err
		}
		targets := names
		if len(targets) == 0 {
			targets = st.RepoNames()
		}
		for _, name := range targets {
			repo, ok := st.Repos[name]
			if !ok {
				results = append(results, RepoResult{
					Name: name,
					Err:  fmt.Errorf("repository %q is not registered (see cepm list)", name),
				})
				continue
			}
			res := updateRepo(ctx, name, repo, opts)
			repo.LastPull = time.Now()
			if res.Err != nil {
				repo.LastError = res.Err.Error()
			} else {
				repo.LastError = ""
			}
			results = append(results, res)
		}
		return st.Save()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func updateRepo(ctx context.Context, name string, r *state.Repo, opts Options) RepoResult {
	res := RepoResult{Name: name}
	dir, err := RepoDir(name)
	if err != nil {
		res.Err = err
		return res
	}
	repo := gitx.Repo{Dir: dir}

	dirty, err := repo.IsDirty(ctx)
	if err != nil {
		res.Err = err
		return res
	}
	if dirty && !opts.StashDirty {
		res.Skipped = true
		res.SkipReason = "working tree has local changes (use --force or set git.stash_dirty)"
		return res
	}

	oldHead := r.Head
	if h, err := repo.Head(ctx); err == nil {
		oldHead = h
	}
	res.OldRef = displayRef(r, oldHead)

	if err := repo.Fetch(ctx); err != nil {
		res.Err = fmt.Errorf("fetch: %w", err)
		return res
	}

	if dirty {
		if err := repo.StashPush(ctx); err != nil {
			res.Err = fmt.Errorf("stash: %w", err)
			return res
		}
	}
	moveErr := moveToLatest(ctx, repo, r, &res)
	if dirty {
		if err := repo.StashPop(ctx); err != nil {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("stash pop failed, your changes are kept in the stash (git -C %s stash pop): %v", dir, err))
		}
	}
	if moveErr != nil {
		res.Err = moveErr
		return res
	}

	newHead, err := repo.Head(ctx)
	if err != nil {
		res.Err = err
		return res
	}
	res.NewRef = displayRef(r, newHead)
	if newHead == oldHead || oldHead == "" {
		r.Head = newHead
		refreshExtensions(name, r, dir, &res, nil)
		return res
	}
	res.Updated = true

	changedFiles, err := repo.ChangedFiles(ctx, oldHead, newHead)
	if err != nil {
		res.Err = err
		return res
	}
	r.Head = newHead
	refreshExtensions(name, r, dir, &res, changedFiles)
	return res
}

// moveToLatest advances the working tree according to the repo's track mode.
func moveToLatest(ctx context.Context, repo gitx.Repo, r *state.Repo, res *RepoResult) error {
	if r.Track == state.TrackTag {
		pattern := r.TagPattern
		if pattern == "" {
			pattern = "v*"
		}
		tags, err := repo.TagsByCreation(ctx, pattern)
		if err != nil {
			return err
		}
		if len(tags) == 0 {
			return fmt.Errorf("no tags match pattern %q", pattern)
		}
		latest, warn := LatestTag(tags)
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
		if latest == r.Tag {
			return nil
		}
		if err := repo.CheckoutDetached(ctx, latest); err != nil {
			return fmt.Errorf("checkout tag %s: %w", latest, err)
		}
		r.Tag = latest
		return nil
	}
	branch := r.Branch
	if branch == "" {
		return fmt.Errorf("no branch recorded for repo (state.json is inconsistent; reinstall the repo)")
	}
	if err := repo.MergeFFOnly(ctx, "origin/"+branch); err != nil {
		return fmt.Errorf("cannot fast-forward %s (history rewritten?): %w", branch, err)
	}
	return nil
}

// LatestTag picks the newest tag from a creatordate-descending list. When all
// tags parse as semver they are compared as versions; otherwise the most
// recently created tag wins and a warning explains the fallback.
func LatestTag(tags []string) (latest string, warning string) {
	type cand struct{ name, canon string }
	cands := make([]cand, 0, len(tags))
	allSemver := true
	for _, t := range tags {
		v := t
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		if semver.IsValid(v) {
			cands = append(cands, cand{t, v})
		} else {
			allSemver = false
		}
	}
	if allSemver && len(cands) > 0 {
		sort.Slice(cands, func(i, j int) bool { return semver.Compare(cands[i].canon, cands[j].canon) > 0 })
		return cands[0].name, ""
	}
	if len(cands) > 0 {
		return tags[0], fmt.Sprintf("tags mix semver and non-semver names; using most recently created tag %q", tags[0])
	}
	return tags[0], ""
}

// refreshExtensions re-scans the repo, updates r.Extensions, and fills
// res.Changed/Added/Removed. changedFiles may be nil (no revision change).
func refreshExtensions(name string, r *state.Repo, dir string, res *RepoResult, changedFiles []string) {
	exts, err := scan.Detect(dir)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("extension scan failed: %v", err))
		return
	}
	old := make(map[string]state.Extension, len(r.Extensions))
	for _, e := range r.Extensions {
		old[e.Dir] = e
	}
	var newList []state.Extension
	for _, e := range exts {
		abs := filepath.Join(dir, e.Dir)
		id, err := extid.FromPath(abs)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("compute ID for %s: %v", abs, err))
			continue
		}
		se := state.Extension{Dir: e.Dir, Name: e.Name, ID: id}
		newList = append(newList, se)
		if _, existed := old[e.Dir]; !existed {
			res.Added = append(res.Added, ExtChange{RepoName: name, Dir: e.Dir, AbsDir: abs, ID: id, Name: e.Name})
		}
		delete(old, e.Dir)
	}
	for _, e := range old {
		res.Removed = append(res.Removed, e)
	}
	r.Extensions = newList

	if len(changedFiles) == 0 {
		return
	}
	changedDirs := map[string]bool{}
	for _, f := range changedFiles {
		if ext, ok := attribute(f, newList); ok {
			changedDirs[ext.Dir] = true
		}
	}
	for _, e := range newList {
		if changedDirs[e.Dir] && !containsDir(res.Added, e.Dir) {
			res.Changed = append(res.Changed, ExtChange{
				RepoName: name, Dir: e.Dir, AbsDir: filepath.Join(dir, e.Dir), ID: e.ID, Name: e.Name,
			})
		}
	}
}

// attribute maps a changed file to the extension owning it, preferring the
// longest matching directory prefix. Dir "." matches every file.
func attribute(file string, exts []state.Extension) (state.Extension, bool) {
	var best state.Extension
	bestLen := -1
	for _, e := range exts {
		if e.Dir == "." {
			if bestLen < 0 {
				best, bestLen = e, 0
			}
			continue
		}
		prefix := e.Dir + "/"
		if strings.HasPrefix(file, prefix) && len(e.Dir) > bestLen {
			best, bestLen = e, len(e.Dir)
		}
	}
	return best, bestLen >= 0
}

func containsDir(changes []ExtChange, dir string) bool {
	for _, c := range changes {
		if c.Dir == dir {
			return true
		}
	}
	return false
}

func displayRef(r *state.Repo, head string) string {
	if r.Track == state.TrackTag && r.Tag != "" {
		return r.Tag
	}
	if len(head) > 12 {
		return head[:12]
	}
	return head
}

// RepoDir returns the working-tree path for a registered repo name.
func RepoDir(name string) (string, error) {
	reposDir, err := paths.ReposDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(reposDir, name), nil
}
