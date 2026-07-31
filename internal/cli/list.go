package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/gitx"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/updater"
)

func newListCmd() *cobra.Command {
	var asJSON bool
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
			chromeStatus := fetchChromeStatus(cmd.Context())
			if asJSON {
				return printListJSON(cmd, st, chromeStatus)
			}
			return printListTable(cmd, st, chromeStatus)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
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
	if r.Track == state.TrackTag {
		return fmt.Sprintf("tag:%s", r.Tag)
	}
	return fmt.Sprintf("branch:%s", r.Branch)
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
	fmt.Fprintln(w, "REPO\tTRACK\tEXTENSION\tID\tDIR\tSTATUS")
	for _, name := range st.RepoNames() {
		r := st.Repos[name]
		for _, e := range r.Extensions {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				name, trackRef(r), e.Name, e.ID, e.Dir, extStatus(chromeStatus, e))
		}
		for _, s := range r.Stale {
			fmt.Fprintf(w, "%s\t\t%s\t%s\t(%s)\tstale — run: cepm cleanup\n", name, s.Name, s.ID, s.Reason)
		}
	}
	for _, o := range st.Orphans {
		fmt.Fprintf(w, "(uninstalled)\t\t%s\t%s\t(%s)\tstale — run: cepm cleanup\n", o.Name, o.ID, o.Reason)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// Errors go below the table: they are multi-line git output, which would
	// otherwise split rows and destroy the column alignment.
	for _, name := range st.RepoNames() {
		if e := st.Repos[name].LastError; e != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "\n⚠ %s: last update failed: %s\n", name, oneLine(e))
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
