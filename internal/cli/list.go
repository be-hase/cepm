package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/gitx"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
	"github.com/be-hase/cepm/internal/updater"
)

func newListCmd() *cobra.Command {
	var asJSON, share bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List registered repositories and extensions",
		GroupID: "ext",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			if share {
				// No Chrome round-trip: what you share must not depend on
				// whether Chrome happens to be running.
				return printListShare(cmd, st)
			}
			chromeStatus := fetchChromeStatus(cmd.Context())
			if asJSON {
				return printListJSON(cmd, st, chromeStatus)
			}
			return printListTable(cmd, st, chromeStatus)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	cmd.Flags().BoolVar(&share, "share", false, "print install commands for the enabled extensions, ready to paste")
	cmd.MarkFlagsMutuallyExclusive("json", "share")
	return cmd
}

// printListShare prints one paste-ready install command per repository,
// covering only the enabled extensions: the answer to "which extensions do
// you run, and how do I get them?".
func printListShare(cmd *cobra.Command, st *state.State) error {
	out := cmd.OutOrStdout()
	shared := 0
	for _, name := range st.RepoNames() {
		r := st.Repos[name]
		var names, dirs []string
		for _, e := range r.Extensions {
			if e.Enabled() {
				names = append(names, term.Safe(e.Name))
				dirs = append(dirs, e.Dir)
			}
		}
		if len(names) == 0 {
			continue
		}
		shared++
		fmt.Fprintf(out, "# %s\n", strings.Join(names, ", "))
		// list renders broken states, but a paste-ready command must not
		// convert one into a plausible different setup on another machine:
		// no runnable URL or no nameable tracking fails closed.
		url, urlOK := shareURL(r.URL)
		track, trackOK := shareTrackFlags(r)
		if !urlOK || !trackOK {
			fmt.Fprintln(out, "# (not shareable: this repository's recorded URL or tracking is unusable)")
			continue
		}
		fmt.Fprintf(out, "cepm install %s", term.Quote(url))
		// The name matters when it does not match what install would derive:
		// a colleague installing two shared repos whose URLs end in the same
		// path segment would otherwise collide on the derived name.
		if name != repoNameFromURL(url) {
			fmt.Fprintf(out, " --name %s", term.Quote(name))
		}
		fmt.Fprint(out, track)
		// A single-extension repo installs without a prompt; only a repo
		// with more needs --only to reproduce this selection.
		if len(r.Extensions) > 1 {
			fmt.Fprintf(out, " --only %s", term.Quote(csvJoin(dirs)))
		}
		fmt.Fprintln(out)
	}
	if shared == 0 {
		fmt.Fprintln(out, "No enabled extensions to share. See: cepm list")
	}
	return nil
}

// shareTrackFlags builds the explicit tracking flags for an install command.
// Always explicit: install resolves an omitted flag from the repo's
// cepm.toml before falling back to its default, so omission does not mean
// "same as mine" on the receiving side. ok=false when the state does not
// name a complete tracking setup.
func shareTrackFlags(r *state.Repo) (string, bool) {
	switch r.Track {
	case state.TrackTag:
		if r.TagPattern == "" {
			return "", false
		}
		return fmt.Sprintf(" --track tag --tag-pattern %s --prerelease=%t",
			term.Quote(r.TagPattern), r.Prerelease), true
	case state.TrackBranch:
		if r.Branch == "" {
			return "", false
		}
		return fmt.Sprintf(" --track branch --branch %s", term.Quote(r.Branch)), true
	default:
		return "", false
	}
}

// shareURL rebuilds the repository URL without its secret-bearing parts —
// userinfo, query, fragment — as something a colleague can actually run,
// where RedactURL's "***" form is display-only. ok=false when no runnable
// URL can be built; like RedactURL, an unparsable URL fails closed.
func shareURL(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	// SCP form ("git@host:path") and local paths: the user part is an
	// account name, not a credential — RedactURL passes them through too.
	if !strings.Contains(raw, "://") {
		return raw, true
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return "", false
	}
	u.User = nil
	u.ForceQuery = false
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	return u.String(), true
}

// csvJoin encodes dirs the way pflag decodes a --only value: as one CSV
// record, so a directory name containing a comma survives the trip.
func csvJoin(dirs []string) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write(dirs) // strings.Builder cannot fail
	w.Flush()
	return strings.TrimSuffix(b.String(), "\n")
}

// fetchChromeStatus returns loaded-extension info by ID, or nil when the host
// is unreachable (Chrome closed / helper not loaded).
func fetchChromeStatus(ctx context.Context) map[string]ipc.ChromeExt {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	exts, err := ipc.ListChrome(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]ipc.ChromeExt, len(exts))
	for _, e := range exts {
		m[e.ID] = e
	}
	return m
}

func extStatus(chromeStatus map[string]ipc.ChromeExt, e state.Extension) string {
	if chromeStatus == nil {
		if !e.Enabled() {
			return "available"
		}
		return "unknown (host not connected)"
	}
	ce, loaded := chromeStatus[e.ID]
	switch {
	case !e.Enabled() && !loaded:
		return "available"
	case !e.Enabled() && loaded:
		return "loaded but not enabled in cepm — cepm enable?"
	case !loaded:
		return "NOT LOADED — load it or run: cepm disable"
	case !ce.Enabled:
		return "loaded (disabled in Chrome)"
	default:
		return "loaded"
	}
}

func trackRef(r *state.Repo) string {
	// Branch and tag names come from the remote; Validate rejects control
	// characters in a state it accepts, but list renders broken states too.
	if r.Track == state.TrackTag {
		return fmt.Sprintf("tag:%s", term.Safe(r.Tag))
	}
	return fmt.Sprintf("branch:%s", term.Safe(r.Branch))
}

func printListTable(cmd *cobra.Command, st *state.State, chromeStatus map[string]ipc.ChromeExt) error {
	if len(st.Repos) == 0 && len(st.Orphans) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No repositories registered. Run: cepm install <git-url>")
		return nil
	}
	if len(st.Repos) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No repositories registered.")
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tTRACK\tEXTENSION\tID\tDIR\tSTATUS\tURL")
	for _, name := range st.RepoNames() {
		r := st.Repos[name]
		// The URL is what a colleague needs to install the same extension;
		// last column, so its length does not push the narrow ones apart.
		for _, e := range r.Extensions {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				name, trackRef(r), e.Name, e.ID, term.Safe(e.Dir), extStatus(chromeStatus, e),
				term.Safe(gitx.RedactURL(r.URL)))
		}
		for _, s := range r.Stale {
			fmt.Fprintf(w, "%s\t\t%s\t%s\t(%s)\tstale — run: cepm cleanup\n", name, s.Name, s.ID, term.Safe(s.Reason))
		}
	}
	for _, o := range st.Orphans {
		fmt.Fprintf(w, "(uninstalled)\t\t%s\t%s\t(%s)\tstale — run: cepm cleanup\n", o.Name, o.ID, term.Safe(o.Reason))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// Errors go below the table: they are multi-line git output, which would
	// otherwise split rows and destroy the column alignment.
	for _, name := range st.RepoNames() {
		if e := st.Repos[name].LastError; e != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "\n⚠ %s: last update failed: %s\n", name, term.Safe(oneLine(e)))
		}
	}
	return nil
}

func printListJSON(cmd *cobra.Command, st *state.State, chromeStatus map[string]ipc.ChromeExt) error {
	type extOut struct {
		Name    string `json:"name"`
		Dir     string `json:"dir"`
		AbsDir  string `json:"absDir"`
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
		Status  string `json:"status"`
	}
	type staleOut struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Reason string `json:"reason"`
		NewDir string `json:"newDir,omitempty"`
	}
	type repoOut struct {
		Name       string     `json:"name"`
		URL        string     `json:"url"`
		Track      string     `json:"track"`
		Branch     string     `json:"branch,omitempty"`
		Tag        string     `json:"tag,omitempty"`
		TagPattern string     `json:"tagPattern,omitempty"`
		Prerelease bool       `json:"prerelease,omitempty"`
		Head       string     `json:"head"`
		LastPull   time.Time  `json:"lastPull"`
		LastError  string     `json:"lastError,omitempty"`
		Extensions []extOut   `json:"extensions"`
		Stale      []staleOut `json:"stale,omitempty"`
	}
	repos := []repoOut{} // never null: scripts iterate this
	for _, name := range st.RepoNames() {
		r := st.Repos[name]
		dir, err := updater.RepoDir(name)
		if err != nil {
			return err
		}
		ro := repoOut{
			Name: name, URL: gitx.RedactURL(r.URL), Track: r.Track, Branch: r.Branch,
			Tag: r.Tag, TagPattern: r.TagPattern, Prerelease: r.Prerelease, Head: r.Head,
			LastPull: r.LastPull, LastError: r.LastError,
			Extensions: []extOut{},
		}
		for _, e := range r.Extensions {
			ro.Extensions = append(ro.Extensions, extOut{
				Name: e.Name, Dir: e.Dir, AbsDir: filepath.Join(dir, e.Dir),
				ID: e.ID, Enabled: e.Enabled(), Status: extStatus(chromeStatus, e),
			})
		}
		for _, s := range r.Stale {
			ro.Stale = append(ro.Stale, staleOut{ID: s.ID, Name: s.Name, Reason: s.Reason, NewDir: s.NewDir})
		}
		repos = append(repos, ro)
	}
	orphans := []staleOut{}
	for _, o := range st.Orphans {
		orphans = append(orphans, staleOut{ID: o.ID, Name: o.Name, Reason: o.Reason, NewDir: o.NewDir})
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"repos": repos, "orphans": orphans})
}
