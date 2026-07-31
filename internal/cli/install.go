package cli

import (
	"context"
	"fmt"
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
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists; pick another --name or run: cepm uninstall %s", dir, name)
	}

	st, err := state.Load()
	if err != nil {
		return err
	}
	if _, exists := st.Repos[name]; exists {
		return fmt.Errorf("repository %q is already registered", name)
	}

	fmt.Fprintf(out, "Cloning %s ...\n", gitx.RedactURL(url))
	if err := gitx.Clone(ctx, url, dir, branch); err != nil {
		return err
	}
	rollback := func() { _ = os.RemoveAll(dir) }

	repo, err := buildRepo(ctx, url, dir, branch, track, tagPattern, flags.prerelease, flags.prereleaseSet)
	if err != nil {
		rollback()
		return err
	}
	if len(repo.Extensions) == 0 {
		rollback()
		return fmt.Errorf("no Chrome extensions found in %s (no manifest.json; repo authors can declare directories in cepm.toml)",
			gitx.RedactURL(url))
	}
	if err := applySelection(cmd, name, repo, flags); err != nil {
		rollback()
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
		// One owner per extension id: a second copy of an extension that
		// pins its id with a manifest "key" would fight over the same Chrome
		// entity as the first.
		for _, e := range repo.Extensions {
			for _, other := range st.RepoNames() {
				for _, oe := range st.Repos[other].Extensions {
					if oe.ID == e.ID {
						return fmt.Errorf("extension %q would get id %s, which repository %q already registers "+
							"(both pin the same manifest \"key\"); uninstall it first", e.Name, e.ID, other)
					}
				}
			}
		}
		st.Repos[name] = repo
		return st.Save()
	})
	if err != nil {
		rollback()
		return err
	}

	fmt.Fprintf(out, "\nInstalled %q", name)
	if repo.Track == state.TrackTag {
		fmt.Fprintf(out, " (tracking tags %q, currently %s)", repo.TagPattern, repo.Tag)
	} else {
		fmt.Fprintf(out, " (tracking branch %s)", repo.Branch)
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
			return fmt.Errorf("--only: no extension found at %v (detected: %v)", missing, extensionDirs(repo))
		}
	case flags.all || len(repo.Extensions) <= 1 || !assist.IsTTY():
		// everything enabled (the default zero value)
	default:
		items := make([]string, len(repo.Extensions))
		for i, e := range repo.Extensions {
			items[i] = fmt.Sprintf("%-24s %s", e.Name, e.Dir)
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

func extensionDirs(repo *state.Repo) []string {
	dirs := make([]string, len(repo.Extensions))
	for i, e := range repo.Extensions {
		dirs[i] = e.Dir
	}
	return dirs
}

// buildRepo inspects a fresh clone and produces its state entry, resolving the
// tracking mode (CLI flags > repo cepm.toml > branch) and, in tag mode,
// checking out the latest matching tag.
func buildRepo(ctx context.Context, url, dir, branch, track, tagPattern string, prerelease, prereleaseSet bool) (*state.Repo, error) {
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
		id, err := extid.ForExtension(filepath.Join(dir, e.Dir), e.Key)
		if err != nil {
			return nil, err
		}
		r.Extensions = append(r.Extensions, state.Extension{Dir: e.Dir, Name: e.Name, Key: e.Key, ID: id})
	}
	return r, nil
}

// repoNameFromURL derives a directory name from a git URL:
// "git@host:team/mytools.git" or "https://host/team/mytools" → "mytools".
func repoNameFromURL(url string) string {
	s := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}
