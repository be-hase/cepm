// Package updater pulls managed repositories and determines which extensions
// changed. It is the single code path shared by "cepm update" (CLI) and the
// native host's periodic loop; concurrent runs are serialized with a file
// lock.
package updater

import (
	"context"
	"fmt"
	"log/slog"
	"path"
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
	Key      string // manifest "key", when the extension pins its ID
	// ManifestChanged marks updates that touched manifest.json. Reloading an
	// unpacked extension through chrome.management.setEnabled re-reads its
	// code from disk but keeps the manifest Chrome cached at install time, so
	// such changes only take effect after Chrome restarts (or the user clicks
	// Reload in chrome://extensions).
	ManifestChanged bool
}

// RenameChange records an extension whose directory moved. Chrome derives
// extension IDs from paths, so a rename mints a new identity: the enabled
// flag carries over, but the user must re-load the new dir and clean up the
// old Chrome entry.
type RenameChange struct {
	Name    string
	OldDir  string
	NewDir  string
	AbsDir  string // absolute path of the new dir
	OldID   string
	NewID   string
	Enabled bool
}

// IdentityChange records an extension whose Chrome id changed while staying
// in the same directory (its manifest "key" was added, removed or edited).
type IdentityChange struct {
	Name    string
	Dir     string
	AbsDir  string
	OldID   string
	NewID   string
	Enabled bool
}

// RepoResult reports the outcome of updating one repository.
type RepoResult struct {
	Name         string
	OldRef       string // commit or tag before the update
	NewRef       string // commit or tag after the update
	Updated      bool   // the working tree moved to a new revision
	Skipped      bool
	SkipReason   string
	Warnings     []string
	Err          error
	Changed      []ExtChange       // enabled extensions whose files changed
	Added        []ExtChange       // newly detected extensions (registered as available)
	Renamed      []RenameChange    // extensions whose directory moved
	Reidentified []IdentityChange  // extensions whose Chrome id changed in place
	Removed      []state.Extension // extensions that disappeared from the repo
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
			res := updateRepo(ctx, name, repo, opts, liveIDsExcept(st, name))
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

// liveIDsExcept maps every registered extension id to its repo, skipping one
// repo — the set a refresh of that repo must not collide with.
func liveIDsExcept(st *state.State, skip string) map[string]string {
	taken := map[string]string{}
	for _, name := range st.RepoNames() {
		if name == skip {
			continue
		}
		for _, e := range st.Repos[name].Extensions {
			taken[e.ID] = name
		}
	}
	return taken
}

func updateRepo(ctx context.Context, name string, r *state.Repo, opts Options, takenIDs map[string]string) RepoResult {
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

	// Before pulling: teach state about keys it may predate. Afterwards a
	// difference between the stored id and the computed one unambiguously
	// means the manifest changed in this update, not that cepm used to ignore
	// the "key" field.
	backfillKeys(r, dir)

	oldHead := r.Head
	if h, err := repo.Head(ctx); err == nil {
		oldHead = h
	}
	res.OldRef = displayRef(r, oldHead)

	if err := repo.Fetch(ctx); err != nil {
		res.Err = fmt.Errorf("fetch: %w", err)
		return res
	}

	stashed := false
	if dirty {
		if stashed, err = repo.StashPush(ctx); err != nil {
			res.Err = fmt.Errorf("stash: %w", err)
			return res
		}
	}
	moveErr := moveToLatest(ctx, repo, r, &res)
	if stashed {
		if err := repo.StashPop(ctx); err != nil {
			// The tree now holds conflict markers, so re-scanning it would
			// misread manifests and unregister extensions. Stop here and let
			// the user resolve it.
			res.Err = fmt.Errorf("restoring your local changes failed; resolve the conflict in %s "+
				"(your changes are still in the stash: git -C %s stash pop): %w", dir, dir, err)
			return res
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
	slog.Debug("repo moved", "repo", name, "from", res.OldRef, "to", res.NewRef, "track", r.Track)
	if newHead == oldHead || oldHead == "" {
		r.Head = newHead
		refreshExtensions(name, r, dir, &res, nil, takenIDs)
		return res
	}
	res.Updated = true

	changedFiles, err := repo.ChangedFiles(ctx, oldHead, newHead)
	if err != nil {
		res.Err = err
		return res
	}
	r.Head = newHead
	refreshExtensions(name, r, dir, &res, changedFiles, takenIDs)
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
		latest, warn := LatestTag(tags, r.Prerelease)
		if latest == "" {
			return fmt.Errorf("no release tag to follow: %s", warn)
		}
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
		if latest == r.Tag {
			// The same name can point somewhere new: releases get re-tagged,
			// and the fetch above accepts that. Compare commits, not names.
			want, err := repo.CommitOf(ctx, latest)
			if err != nil {
				return err
			}
			head, err := repo.Head(ctx)
			if err != nil {
				return err
			}
			if want == head {
				return nil
			}
			res.Warnings = append(res.Warnings, fmt.Sprintf("tag %s now points at a different commit", latest))
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
	// Merging into whatever the user happens to have checked out could
	// fast-forward their own branch onto the tracked one; refuse instead.
	current, err := repo.CurrentBranch(ctx)
	if err != nil {
		return err
	}
	if current != branch {
		where := "a detached HEAD"
		if current != "" {
			where = "branch " + current
		}
		return fmt.Errorf("the clone is on %s but cepm tracks %s; switch back with: git -C %s checkout %s",
			where, branch, repo.Dir, branch)
	}
	if err := repo.MergeFFOnly(ctx, "origin/"+branch); err != nil {
		return fmt.Errorf("cannot fast-forward %s (history rewritten?): %w", branch, err)
	}
	return nil
}

// LatestTag picks the highest semver tag, ignoring tags that are not version
// numbers (with a warning) and prereleases (v2.0.0-rc1, v1.5.0-beta.2) unless
// includePrerelease is set. It returns "" plus an explanation when there is
// nothing safe to follow.
//
// This tracks version tags, which is close to but not the same as tracking
// GitHub Releases: publishing a release pushes its tag and a draft pushes
// none, but a tag pushed without a release is followed too, and a release's
// prerelease flag is only visible here through the tag name.
func LatestTag(tags []string, includePrerelease bool) (latest string, warning string) {
	type cand struct{ name, canon string }
	var cands []cand
	nonSemver, skippedPrerelease := 0, 0
	for _, t := range tags {
		v := t
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		switch {
		case !semver.IsValid(v):
			nonSemver++
		case semver.Prerelease(v) != "" && !includePrerelease:
			skippedPrerelease++
		default:
			cands = append(cands, cand{t, v})
		}
	}
	if len(cands) > 0 {
		sort.Slice(cands, func(i, j int) bool { return semver.Compare(cands[i].canon, cands[j].canon) > 0 })
		if nonSemver > 0 {
			warning = fmt.Sprintf("ignored %d tag(s) that are not version numbers; following %q", nonSemver, cands[0].name)
		}
		return cands[0].name, warning
	}
	if skippedPrerelease > 0 {
		return "", fmt.Sprintf("only prerelease tags match (%d); use --prerelease to follow them", skippedPrerelease)
	}
	// Refuse rather than guess: following "whichever tag was created last"
	// would ship whatever someone tagged, which is the opposite of tracking
	// releases.
	return "", fmt.Sprintf("no version-number tags match (found %d tag(s) that are not semver, e.g. %q); "+
		"tag releases as v1.2.3, or track a branch instead", nonSemver, tags[0])
}

// refreshExtensions re-scans the repo, updates r.Extensions (preserving the
// user's enabled/available intent), and fills res.Changed/Added/Renamed/
// Removed. Renames are inferred by matching manifest names between vanished
// and appeared directories; removed or renamed-away entries are recorded as
// stale so "cepm cleanup" can clear the broken Chrome copies later.
// changedFiles may be nil (no revision change).
func refreshExtensions(name string, r *state.Repo, dir string, res *RepoResult, changedFiles []string, takenIDs map[string]string) {
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
	var added []ExtChange
	for _, e := range exts {
		abs := filepath.Join(dir, e.Dir)
		id, err := extid.ForExtension(abs, e.Key)
		if err != nil {
			// All or nothing: dropping just this one would unregister a
			// working extension (losing its enable choice and marking it for
			// cleanup) because a pulled commit has a malformed "key".
			res.Err = fmt.Errorf("%s: %w", e.Dir, err)
			return
		}
		if owner, taken := takenIDs[id]; taken {
			// Chrome has one entity per id: two live registrations would
			// fight over it (uninstalling one removes the other's extension).
			// Refuse the whole refresh, keeping the old state. takenIDs also
			// accumulates this repo's own directories, so two of them pinning
			// the same key is caught here too.
			res.Err = fmt.Errorf("%s would get extension id %s, which %s already registers "+
				"(both pin the same manifest \"key\")", e.Dir, id, owner)
			return
		}
		takenIDs[id] = name + "/" + e.Dir
		se := state.Extension{Dir: e.Dir, Name: e.Name, Key: e.Key, ID: id}
		if prev, existed := old[e.Dir]; existed {
			se.Disabled = prev.Disabled
			if prev.ID != id {
				noteIdentityChange(name, dir, prev, se, res)
				r.AddStale(state.StaleExtension{ID: prev.ID, Name: prev.Name, Reason: "id changed"})
			}
		} else {
			// New extensions arrive as "available"; the user opts in with
			// cepm enable (renames inherit the old intent below).
			se.Disabled = true
			added = append(added, ExtChange{RepoName: name, Dir: e.Dir, AbsDir: abs, ID: id, Name: e.Name, Key: e.Key})
		}
		newList = append(newList, se)
		delete(old, e.Dir)
	}

	// Rename inference: a vanished dir and an appeared dir that identify the
	// same extension is a move. A manifest "key" is definitive; otherwise the
	// manifest name has to be unique on *both* sides, or we cannot tell which
	// new directory a removed one became.
	removed := old
	for i := range added {
		match, ok := matchRenamed(added[i], removed, added)
		if !ok {
			continue
		}
		for j := range newList {
			if newList[j].Dir == added[i].Dir {
				newList[j].Disabled = match.Disabled
			}
		}
		res.Renamed = append(res.Renamed, RenameChange{
			Name: match.Name, OldDir: match.Dir, NewDir: added[i].Dir,
			AbsDir: added[i].AbsDir, OldID: match.ID, NewID: added[i].ID,
			Enabled: match.Enabled(),
		})
		r.AddStale(state.StaleExtension{ID: match.ID, Name: match.Name, Reason: "renamed", NewDir: added[i].Dir})
		delete(removed, match.Dir)
		added[i].Name = "" // consumed by the rename
	}
	for _, a := range added {
		if a.Name != "" {
			res.Added = append(res.Added, a)
		}
	}
	for _, e := range removed {
		res.Removed = append(res.Removed, e)
		r.AddStale(state.StaleExtension{ID: e.ID, Name: e.Name, Reason: "removed"})
	}
	r.Extensions = newList
	// Extension IDs are derived from paths, so reverting a deletion or a
	// rename brings the old ID back to life. Clearing those keeps "cepm
	// cleanup" from uninstalling an extension that works again.
	for _, e := range newList {
		r.RemoveStale(e.ID)
	}

	if len(changedFiles) == 0 {
		return
	}
	changedDirs := map[string]bool{}
	manifestChanged := map[string]bool{}
	for _, f := range changedFiles {
		ext, ok := attribute(f, newList)
		if !ok {
			continue
		}
		changedDirs[ext.Dir] = true
		if f == path.Join(filepath.ToSlash(ext.Dir), "manifest.json") || (ext.Dir == "." && f == "manifest.json") {
			manifestChanged[ext.Dir] = true
		}
	}
	for _, e := range newList {
		if changedDirs[e.Dir] && e.Enabled() && !containsDir(res.Added, e.Dir) && !renameTarget(res.Renamed, e.Dir) {
			res.Changed = append(res.Changed, ExtChange{
				RepoName: name, Dir: e.Dir, AbsDir: filepath.Join(dir, e.Dir), ID: e.ID, Name: e.Name,
				ManifestChanged: manifestChanged[e.Dir],
			})
		}
	}
}

// backfillKeys records manifest keys for extensions registered by a cepm that
// did not read them, correcting the id at the same time: those entries stored
// a path-derived id that Chrome never used. It reads the working tree as it
// is *before* an update, so it can only ever describe the state cepm already
// had.
func backfillKeys(r *state.Repo, dir string) {
	for i := range r.Extensions {
		e := &r.Extensions[i]
		if e.Key != "" {
			continue
		}
		m, err := scan.ReadManifest(filepath.Join(dir, e.Dir))
		if err != nil || m.Key == "" {
			continue
		}
		id, err := extid.ForExtension(filepath.Join(dir, e.Dir), m.Key)
		if err != nil {
			continue
		}
		e.Key, e.ID = m.Key, id
	}
}

// noteIdentityChange records that an extension kept its directory but got a
// new Chrome id — which happens when a manifest gains, loses or changes its
// "key". Chrome treats that as a different extension, so the old entry is now
// dead and the new one needs loading once.
func noteIdentityChange(repoName, repoDir string, prev, now state.Extension, res *RepoResult) {
	res.Reidentified = append(res.Reidentified, IdentityChange{
		Name: now.Name, Dir: now.Dir, AbsDir: filepath.Join(repoDir, now.Dir),
		OldID: prev.ID, NewID: now.ID, Enabled: now.Enabled(),
	})
}

// matchRenamed decides whether the appeared extension is a removed one that
// moved. Names are display strings (sanitised and truncated, so distinct
// names can collide), which is why a key match wins and an ambiguous name
// match is refused rather than guessed.
func matchRenamed(appeared ExtChange, removed map[string]state.Extension, allAdded []ExtChange) (state.Extension, bool) {
	if appeared.Key != "" {
		var found state.Extension
		count := 0
		for _, e := range removed {
			if e.Key == appeared.Key {
				found, count = e, count+1
			}
		}
		if count == 1 {
			return found, true
		}
		return state.Extension{}, false
	}

	var found state.Extension
	removedCount := 0
	for _, e := range removed {
		if e.Key == "" && e.Name == appeared.Name {
			found, removedCount = e, removedCount+1
		}
	}
	if removedCount != 1 {
		return state.Extension{}, false
	}
	// The name must also be unique among the new directories: otherwise which
	// of them inherited the old one's enable/disable choice is a coin flip.
	addedCount := 0
	for _, a := range allAdded {
		if a.Name == appeared.Name {
			addedCount++
		}
	}
	if addedCount != 1 {
		return state.Extension{}, false
	}
	return found, true
}

func renameTarget(renames []RenameChange, dir string) bool {
	for _, rn := range renames {
		if rn.NewDir == dir {
			return true
		}
	}
	return false
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
