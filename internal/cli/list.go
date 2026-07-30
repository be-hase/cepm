package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

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

func extStatus(chromeStatus map[string]ipc.ChromeExt, id string) string {
	if chromeStatus == nil {
		return "unknown (host not connected)"
	}
	e, ok := chromeStatus[id]
	switch {
	case !ok:
		return "not loaded"
	case e.Enabled:
		return "loaded"
	default:
		return "loaded (disabled)"
	}
}

func trackRef(r *state.Repo) string {
	if r.Track == state.TrackTag {
		return fmt.Sprintf("tag:%s", r.Tag)
	}
	return fmt.Sprintf("branch:%s", r.Branch)
}

func printListTable(cmd *cobra.Command, st *state.State, chromeStatus map[string]ipc.ChromeExt) error {
	if len(st.Repos) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No repositories registered. Run: cepm install <git-url>")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tTRACK\tEXTENSION\tID\tDIR\tSTATUS")
	for _, name := range st.RepoNames() {
		r := st.Repos[name]
		for _, e := range r.Extensions {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				name, trackRef(r), e.Name, e.ID, e.Dir, extStatus(chromeStatus, e.ID))
		}
		if r.LastError != "" {
			fmt.Fprintf(w, "%s\t\t⚠ last update failed: %s\t\t\t\n", name, r.LastError)
		}
	}
	return w.Flush()
}

func printListJSON(cmd *cobra.Command, st *state.State, chromeStatus map[string]ipc.ChromeExt) error {
	type extOut struct {
		Name   string `json:"name"`
		Dir    string `json:"dir"`
		AbsDir string `json:"absDir"`
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	type repoOut struct {
		Name       string    `json:"name"`
		URL        string    `json:"url"`
		Track      string    `json:"track"`
		Branch     string    `json:"branch,omitempty"`
		Tag        string    `json:"tag,omitempty"`
		TagPattern string    `json:"tagPattern,omitempty"`
		Head       string    `json:"head"`
		LastPull   time.Time `json:"lastPull"`
		LastError  string    `json:"lastError,omitempty"`
		Extensions []extOut  `json:"extensions"`
	}
	var repos []repoOut
	for _, name := range st.RepoNames() {
		r := st.Repos[name]
		dir, err := updater.RepoDir(name)
		if err != nil {
			return err
		}
		ro := repoOut{
			Name: name, URL: r.URL, Track: r.Track, Branch: r.Branch,
			Tag: r.Tag, TagPattern: r.TagPattern, Head: r.Head,
			LastPull: r.LastPull, LastError: r.LastError,
		}
		for _, e := range r.Extensions {
			ro.Extensions = append(ro.Extensions, extOut{
				Name: e.Name, Dir: e.Dir, AbsDir: filepath.Join(dir, e.Dir),
				ID: e.ID, Status: extStatus(chromeStatus, e.ID),
			})
		}
		repos = append(repos, ro)
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"repos": repos})
}
