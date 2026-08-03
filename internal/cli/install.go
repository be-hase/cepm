package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/extid"
	"github.com/be-hase/cepm/internal/gitx"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/scan"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
	"github.com/be-hase/cepm/internal/updater"
)

type installFlags struct {
	name          string
	branch        string
	track         string
	tagPattern    string
	prerelease    bool
	prereleaseSet bool // --prerelease was given explicitly (true or false)
	only          []string
	all           bool
}

func newInstallCmd() *cobra.Command {
	var flags installFlags
	cmd := &cobra.Command{
		Use:     "install <git-url>",
		Short:   "Clone a git repository and register its extensions",
		GroupID: "ext",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Distinguish "not given" from an explicit --prerelease=false, so
			// the documented precedence (flag over cepm.toml) holds in both
			// directions.
			flags.prereleaseSet = cmd.Flags().Changed("prerelease")
			return runInstall(cmd, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.name, "name", "", "directory name for the clone (default: derived from the URL)")
	cmd.Flags().StringVar(&flags.branch, "branch", "", "branch to track (branch mode)")
	cmd.Flags().StringVar(&flags.track, "track", "", `tracking mode: "branch" or "tag" (default: repo's cepm.toml, then branch)`)
	cmd.Flags().StringVar(&flags.tagPattern, "tag-pattern", "", `glob for release tags in tag mode (default "v*")`)
	cmd.Flags().BoolVar(&flags.prerelease, "prerelease", false,
		"in tag mode, also follow prerelease versions (v2.0.0-rc1); --prerelease=false overrides the repo's cepm.toml")
	cmd.Flags().StringSliceVar(&flags.only, "only", nil, "enable only these extension dirs (repo-relative); others stay available")
	cmd.Flags().BoolVar(&flags.all, "all", false, "enable every detected extension without asking")
	return cmd
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func runInstall(cmd *cobra.Command, url string, flags installFlags) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	name, branch, track, tagPattern := flags.name, flags.branch, flags.track, flags.tagPattern
	if track != "" && track != state.TrackBranch && track != state.TrackTag {
		return fmt.Errorf(`--track must be "branch" or "tag"`)
	}
	if name == "" {
		name = repoNameFromURL(url)
	}
	if !repoNameRe.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("invalid repository name %q (use --name)", name)
	}
	if err := paths.EnsureLayout(); err != nil {
		return err
	}
	dir, err := updater.RepoDir(name)
	if err != nil {
		return err
	}
	st, err := state.Load()
	if err != nil {
		return err
	}
	if _, exists := st.Repos[name]; exists {
		return fmt.Errorf("repository %q is already registered", name)
	}
	if err := destFree(st, name, dir); err != nil {
		return err
	}

	// Everything up to the lock — the clone, the scan, the interactive
	// selection — happens in a staging directory. The final path only comes
	// into existence inside the lock, together with the state entry: a
	// concurrent reset (which moves repos/ away wholesale) can then never
	// separate a registered repository from its clone.
	//
	// Staged beside repos/, which is normally the same filesystem, so the
	// last step is one atomic rename — and a reset that moves repos/ away
	// while install waits at the prompt leaves the staging untouched.
	//
	// Unless repos/ is a symlink to another volume. A rename across devices
	// fails outright (EXDEV), so for that layout the staging goes inside
	// repos/ instead: install working at all is worth more than surviving a
	// concurrent reset, which for that layout it never did.
	home, err := paths.CepmDir()
	if err != nil {
		return err
	}
	reposDir, err := paths.ReposDir()
	if err != nil {
		return err
	}
	if err := paths.EnsureLayout(); err != nil {
		return err
	}
	// Resolve where the staging will go *before* making it, so its path is
	// real from the first moment. A reset landing during the selection
	// prompt replaces the repos/ symlink, and a logical path would then name
	// a directory on the other side of it: the rename would fail (the
	// accepted trade) and the cleanup would delete the wrong place, leaving
	// a full clone behind on the volume nobody looks at. Resolving after
	// creating would only shrink that window instead of closing it.
	stagingParent := stagingParentFor(home, reposDir)
	if resolved, rerr := filepath.EvalSymlinks(stagingParent); rerr == nil {
		stagingParent = resolved
	}
	staging, err := makeStagingDir(stagingParent, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	stagingClone := filepath.Join(staging, name)

	// Safe as well as redacted: the URL is a raw argument, and an SCP-form
	// one ("git@host:path") passes through RedactURL untouched — with
	// whatever control characters it carries.
	fmt.Fprintf(out, "Cloning %s ...\n", term.Safe(gitx.RedactURL(url)))
	if err := gitx.Clone(ctx, url, stagingClone, branch); err != nil {
		return err
	}

	// ids derive from the *final* path — the one Chrome will load.
	repo, err := buildRepo(ctx, url, stagingClone, dir, branch, track, tagPattern, flags.prerelease, flags.prereleaseSet)
	if err != nil {
		return err
	}
	if len(repo.Extensions) == 0 {
		return fmt.Errorf("no Chrome extensions found in %s (no manifest.json; repo authors can declare directories in cepm.toml)",
			term.Safe(gitx.RedactURL(url)))
	}
	if err := applySelection(cmd, name, repo, flags); err != nil {
		return err
	}

	err = updater.WithLock(ctx, func() error {
		st, err := state.Load()
		if err != nil {
			return err
		}
		if _, exists := st.Repos[name]; exists {
			return fmt.Errorf("repository %q is already registered", name)
		}
		if err := destFree(st, name, dir); err != nil {
			return err
		}
		if err := os.Rename(stagingClone, dir); err != nil {
			return err
		}
		// One owner per extension id — including within this repository, where
		// two directories can pin the same manifest "key".
		st.Repos[name] = repo
		if err := st.Validate(); err != nil {
			return undoRename(fmt.Errorf("cannot install %q: %w", name, err), dir, stagingClone)
		}
		// saveState keeps a durability-only failure from reaching here: the
		// registration would already be on disk, and moving the clone back
		// would leave the repository registered with nothing at the path its
		// ids were derived from.
		if err := saveState(out, st); err != nil {
			// The state on disk did not change, so the filesystem must not
			// either: put the clone back before reporting.
			return undoRename(err, dir, stagingClone)
		}
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "\nInstalled %q", name)
	if repo.Track == state.TrackTag {
		fmt.Fprintf(out, " (tracking tags %q, currently %s)", repo.TagPattern, term.Safe(repo.Tag))
	} else {
		// Safe like every other site that prints a branch: reaching here
		// depends on st.Validate() above having rejected control characters,
		// which is too remote a guard to lean on.
		fmt.Fprintf(out, " (tracking branch %s)", term.Safe(repo.Branch))
	}
	var targets []loadTarget
	available := 0
	for _, e := range repo.Extensions {
		if e.Enabled() {
			targets = append(targets, loadTarget{Name: e.Name, AbsDir: filepath.Join(dir, e.Dir), ID: e.ID})
		} else {
			available++
		}
	}
	fmt.Fprintf(out, " — %d extension(s) enabled", len(targets))
	if available > 0 {
		fmt.Fprintf(out, ", %d available (enable later with: cepm enable %s)", available, name)
	}
	fmt.Fprintln(out)

	runLoadCeremony(ctx, cmd, targets)
	fmt.Fprintf(out, "\nAfter loading, updates are applied automatically.\n")
	return nil
}

// applySelection decides which detected extensions start enabled, from flags
// or an interactive prompt (multi-extension repos on a TTY).
func applySelection(cmd *cobra.Command, repoName string, repo *state.Repo, flags installFlags) error {
	if flags.all && len(flags.only) > 0 {
		return fmt.Errorf("--all and --only are mutually exclusive")
	}
	switch {
	case len(flags.only) > 0:
		want := map[string]bool{}
		for _, d := range flags.only {
			want[filepath.Clean(d)] = true
		}
		for i := range repo.Extensions {
			if !want[repo.Extensions[i].Dir] {
				repo.Extensions[i].Disabled = true
			}
			delete(want, repo.Extensions[i].Dir)
		}
		if len(want) > 0 {
			var missing []string
			for d := range want {
				missing = append(missing, d)
			}
			return fmt.Errorf("--only: no extension found at %v (detected: %v)", missing, repoDirs(repo))
		}
	case flags.all || len(repo.Extensions) <= 1 || !assist.IsTTY():
		// everything enabled (the default zero value)
	default:
		items := make([]string, len(repo.Extensions))
		for i, e := range repo.Extensions {
			items[i] = fmt.Sprintf("%-24s %s", e.Name, term.Safe(e.Dir))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "\n%d extensions found in %s:\n\n", len(items), repoName)
		picked, err := selectByNumbers(cmd, "Enable which?", items)
		if err != nil {
			return err
		}
		enabled := map[int]bool{}
		for _, i := range picked {
			enabled[i] = true
		}
		for i := range repo.Extensions {
			repo.Extensions[i].Disabled = !enabled[i]
		}
	}
	return nil
}

// buildRepo inspects a fresh clone and produces its state entry, resolving the
// tracking mode (CLI flags > repo cepm.toml > branch) and, in tag mode,
// checking out the latest matching tag.
// buildRepo reads the clone at dir; idBase is the path the clone will live
// at, which is what the (path-derived) extension ids must be computed from.
func buildRepo(ctx context.Context, url, dir, idBase, branch, track, tagPattern string, prerelease, prereleaseSet bool) (*state.Repo, error) {
	repoCfg, err := scan.LoadRepoConfig(dir)
	if err != nil {
		return nil, err
	}
	if track == "" && repoCfg != nil && repoCfg.Track != "" {
		track = repoCfg.Track
	}
	if track == "" {
		track = state.TrackBranch
	}
	if tagPattern == "" && repoCfg != nil && repoCfg.TagPattern != "" {
		tagPattern = repoCfg.TagPattern
	}
	if !prereleaseSet && repoCfg != nil {
		prerelease = repoCfg.Prerelease
	}

	g := gitx.Repo{Dir: dir}
	r := &state.Repo{URL: url, Track: track, Prerelease: prerelease}

	switch track {
	case state.TrackTag:
		if r.TagPattern = tagPattern; r.TagPattern == "" {
			r.TagPattern = "v*"
		}
		// The cepm.toml path was validated on load; the --tag-pattern flag
		// arrives here unchecked, and TagsByCreation's contract is that
		// callers validate (a pattern starting with "-" would read as a git
		// option).
		if !scan.ValidTagPattern(r.TagPattern) {
			return nil, fmt.Errorf("invalid tag pattern %q", term.Safe(r.TagPattern))
		}
		tags, err := g.TagsByCreation(ctx, r.TagPattern)
		if err != nil {
			return nil, err
		}
		if len(tags) == 0 {
			return nil, fmt.Errorf("no tags match pattern %q (use --track branch to follow a branch instead)", r.TagPattern)
		}
		latest, warn := updater.LatestTag(tags, prerelease)
		if latest == "" {
			return nil, fmt.Errorf("no release tag to follow: %s", warn)
		}
		if warn != "" {
			fmt.Fprintln(os.Stderr, "Warning:", warn)
		}
		if err := g.CheckoutDetached(ctx, latest); err != nil {
			return nil, err
		}
		r.Tag = latest
	default:
		b := branch
		if b == "" {
			if b, err = g.CurrentBranch(ctx); err != nil {
				return nil, err
			}
		}
		if b == "" {
			return nil, fmt.Errorf("could not determine the default branch; pass --branch")
		}
		r.Branch = b
	}

	if r.Head, err = g.Head(ctx); err != nil {
		return nil, err
	}
	exts, err := scan.Detect(dir)
	if err != nil {
		return nil, err
	}
	if len(exts) > scan.MaxAutoDetect {
		fmt.Fprintf(os.Stderr, "Warning: %d extensions auto-detected; consider declaring them in cepm.toml\n", len(exts))
	}
	for _, e := range exts {
		id, err := extid.ForExtension(filepath.Join(idBase, e.Dir), e.Key)
		if err != nil {
			return nil, err
		}
		r.Extensions = append(r.Extensions, state.Extension{Dir: e.Dir, Name: e.Name, Key: e.Key, ID: id})
	}
	return r, nil
}

// destFree reports whether repos/<name> is available, and — because the two
// situations need opposite advice — distinguishes a clone that belongs to a
// registered repository from one left behind by an interrupted install:
// telling the user to run "cepm uninstall" for the latter is a dead end
// (uninstall does not know the name).
func destFree(st *state.State, name, dir string) error {
	_, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, registered := st.Repos[name]; registered {
		return fmt.Errorf("%s already exists; pick another --name or run: cepm uninstall %s", dir, name)
	}
	return fmt.Errorf("%s exists but no repository is registered for it — likely an interrupted install; "+
		"remove it (rm -rf %s) or run cepm reset, then retry", dir, term.Quote(dir))
}

// makeStagingDir creates the staging directory. Test seam: replacing it is
// the only way to land a reset in the instant between the directory
// existing and the code deciding what its path is — the gap that resolving
// the parent first is there to remove, and that resolving afterwards would
// only have narrowed.
var makeStagingDir = os.MkdirTemp

// stagingParentFor picks where an install stages its clone: beside repos/
// normally, inside it when repos/ lives on another filesystem. See the note
// at the call site for what each choice costs.
func stagingParentFor(home, reposDir string) string {
	if sameFilesystem(home, reposDir) {
		return home
	}
	return reposDir
}

// sameFilesystem reports whether a rename between the two directories can
// work. It answers "yes" when it cannot tell: the caller's fallback trades
// away a guarantee, and guessing that away on a failed stat would be worse
// than the rename failing loudly.
var sameFilesystem = func(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return true
	}
	fb, err := os.Stat(b)
	if err != nil {
		return true
	}
	da, ok := deviceOf(fa)
	if !ok {
		return true
	}
	db, ok := deviceOf(fb)
	if !ok {
		return true
	}
	return da == db
}

// undoRename moves the clone back to staging after a failed registration.
// If even that fails, the world is exactly the half-installed state the
// caller tried to avoid, and the user has to hear about both problems.
func undoRename(cause error, dir, stagingClone string) error {
	if rbErr := os.Rename(dir, stagingClone); rbErr != nil {
		return fmt.Errorf("%w; and moving the clone back failed too (%v) — %s now exists unregistered, remove it before retrying",
			cause, rbErr, dir)
	}
	return cause
}

// repoNameFromURL derives a directory name from a git URL:
// "git@host:team/mytools.git" or "https://host/team/mytools" → "mytools".
func repoNameFromURL(url string) string {
	s := url
	// A query or fragment is not part of the name — and can carry a token,
	// which the invalid-name error would then echo to the terminal.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}
