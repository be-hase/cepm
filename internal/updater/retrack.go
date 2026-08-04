package updater

import (
	"context"
	"fmt"
	"time"

	"github.com/be-hase/cepm/internal/gitx"
	"github.com/be-hase/cepm/internal/scan"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
)

// RetrackOptions is the tracking configuration Retrack moves a repository to.
// Empty Branch/TagPattern keep what the state records (then the usual
// defaults); Prerelease only applies when PrereleaseSet is true, so an
// unspecified flag keeps the recorded choice in both directions.
type RetrackOptions struct {
	Track         string // state.TrackBranch or state.TrackTag
	Branch        string
	TagPattern    string
	Prerelease    bool
	PrereleaseSet bool
}

// Retrack switches how a registered repository follows its remote and moves
// the clone to the new target, reporting the outcome exactly like Update so
// callers can reuse its printing and reload path.
//
// The order inside the lock is deliberate. Everything that can refuse — the
// dirty check, the fetch, validating and resolving the tag or branch — runs
// before the state is touched, so a doomed switch changes nothing. The new
// tracking fields are then saved *before* the clone moves: if the move fails
// after that, state.json names the new mode with the old head, which is a
// state the existing commands describe honestly. In tag mode a plain
// "cepm update" retries the checkout itself; in branch mode update refuses to
// change branches on principle, so its error hands the user the exact git
// switch to run — and re-running "cepm track" performs the same switch. The
// reverse order would be worse on both counts: a clone moved under the *old*
// mode's description, with nothing recorded to say why.
func Retrack(ctx context.Context, name string, opts RetrackOptions) (RepoResult, error) {
	var res RepoResult
	var saveWarning string
	err := WithLock(ctx, func() error {
		st, err := state.LoadValid()
		if err != nil {
			return err
		}
		r, ok := st.Repos[name]
		if !ok {
			return fmt.Errorf("repository %q is not registered (see cepm list)", name)
		}
		dir, err := RepoDir(name)
		if err != nil {
			return err
		}
		repo := gitx.Repo{Dir: dir}

		// A mode switch checks out a different commit, and carrying
		// uncommitted work across that is git's conflict handling, not a
		// package manager's. Unlike update there is no --force: this is a
		// one-off command, and committing or stashing first is cheap.
		dirty, err := repo.IsDirty(ctx)
		if err != nil {
			return err
		}
		if dirty {
			return fmt.Errorf("the working tree at %s has local changes; commit or stash them first", term.Quote(dir))
		}

		// Fetch before deciding: the tags or the branch being switched to may
		// only exist upstream. updateRepo fetches again below; the second one
		// is a no-op on the wire and the price of reusing the one update path.
		if err := repo.Fetch(ctx); err != nil {
			return fmt.Errorf("fetch: %w", err)
		}

		// The tag the repository sits on now, remembered across the switch:
		// updateRepo renders OldRef from the *new* mode, which would reduce
		// "v1.0.0 → main" to a pair of commit hashes.
		prevTag := ""
		if r.Track == state.TrackTag {
			prevTag = r.Tag
		}

		switch opts.Track {
		case state.TrackTag:
			pattern := opts.TagPattern
			if pattern == "" {
				pattern = r.TagPattern
			}
			if pattern == "" {
				pattern = "v*"
			}
			if !scan.ValidTagPattern(pattern) {
				return fmt.Errorf("invalid tag pattern %q", term.Safe(pattern))
			}
			prerelease := r.Prerelease
			if opts.PrereleaseSet {
				prerelease = opts.Prerelease
			}
			tags, err := repo.TagsByCreation(ctx, pattern)
			if err != nil {
				return err
			}
			if len(tags) == 0 {
				return fmt.Errorf("no tags match pattern %q — nothing changed (tag a release upstream, or adjust --tag-pattern)", pattern)
			}
			latest, warn := LatestTag(tags, prerelease)
			if latest == "" {
				return fmt.Errorf("no release tag to follow — nothing changed: %s", warn)
			}
			// warn is dropped here on purpose: updateRepo resolves the tag
			// again below and reports the same warning on the result.
			//
			// A tag can name a blob or a tree; checking it out would only
			// fail after the state committed to it.
			if _, err := repo.CommitOf(ctx, latest); err != nil {
				return fmt.Errorf("tag %s does not name a commit — nothing changed: %w", term.Safe(latest), err)
			}
			r.Track = state.TrackTag
			r.TagPattern = pattern
			r.Prerelease = prerelease
		case state.TrackBranch:
			branch := opts.Branch
			if branch == "" {
				branch = r.Branch
			}
			if branch == "" {
				if branch, err = repo.DefaultBranch(ctx); err != nil {
					return fmt.Errorf("could not determine the default branch (%v); pass --branch", err)
				}
			}
			// Everything that would make the switch fail after the save is
			// checked here instead: a name git resolves but refuses to switch
			// to ("HEAD"), a branch that is not on origin, and a local branch
			// carrying its own commits, which the fast-forward after the
			// switch could never absorb.
			if err := gitx.CheckBranchName(ctx, branch); err != nil {
				return fmt.Errorf("invalid branch name — nothing changed: %w", err)
			}
			// Fully qualified: the short "origin/<branch>" is resolved through
			// the whole ref order, so a *tag* named "origin/<branch>" would
			// answer for a remote branch that does not exist.
			if _, err := repo.CommitOf(ctx, "refs/remotes/origin/"+branch); err != nil {
				return fmt.Errorf("branch %q not found on origin — nothing changed: %w", term.Safe(branch), err)
			}
			ff, err := repo.FastForwardable(ctx, branch)
			if err != nil {
				return err
			}
			if !ff {
				return fmt.Errorf("the local branch %s in %s has commits that origin/%s does not — nothing changed; "+
					"push, rebase or reset the branch first", term.Safe(branch), term.Quote(dir), term.Safe(branch))
			}
			r.Track = state.TrackBranch
			r.Branch = branch
			// Tag means "currently checked-out tag (tag mode)", which stops
			// being true the moment the switch lands.
			r.Tag = ""
		default:
			return fmt.Errorf("track must be %q or %q", state.TrackBranch, state.TrackTag)
		}

		if err := st.Save(); err != nil {
			if !state.Committed(err) {
				return err
			}
			saveWarning = err.Error()
		}

		if r.Track == state.TrackBranch {
			// updateRepo deliberately refuses to change branches (it must
			// never fast-forward whatever the user checked out), so the
			// switch is this command's own step. In tag mode there is nothing
			// to do here: moveToLatest checks the tag out itself.
			if err := repo.SwitchBranch(ctx, r.Branch); err != nil {
				// Not "run cepm update": update refuses to change branches, so
				// only re-running track (or the git switch update would then
				// suggest) can finish this move.
				return fmt.Errorf("switch to branch %s: %w\nThe tracking change is saved; fix the clone at %s, then re-run: cepm track %s branch",
					term.Safe(r.Branch), err, term.Quote(dir), term.Quote(name))
			}
		}

		res = updateRepo(ctx, name, r, Options{}, liveIDsExcept(st, name))
		if prevTag != "" && r.Track == state.TrackBranch {
			res.OldRef = prevTag
		}
		if res.Err == nil && !res.Skipped {
			r.LastPull = time.Now()
		}
		if res.Err != nil {
			r.LastError = term.Strip(res.Err.Error())
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("the tracking change itself is saved; after fixing the problem run: cepm update %s", term.Quote(name)))
		} else {
			r.LastError = ""
		}
		if err := st.Save(); err != nil {
			if !state.Committed(err) {
				return err
			}
			saveWarning = err.Error()
		}
		return nil
	})
	if err != nil {
		return RepoResult{}, err
	}
	if saveWarning != "" {
		res.Warnings = append(res.Warnings, saveWarning)
	}
	return res, nil
}
