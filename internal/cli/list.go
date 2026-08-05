package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"path/filepath"
	"slices"
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
	var asJSON, share, all, full bool
	var statuses []string
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List registered repositories and extensions",
		GroupID: "ext",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// pflag turns --status "" into zero values, which must not
			// silently mean "the default view": an empty $VAR in a script
			// is a mistake, not a selection.
			if cmd.Flags().Changed("status") && len(statuses) == 0 {
				return fmt.Errorf("--status is empty (valid: %s)", strings.Join(listStatuses, ", "))
			}
			filter, err := newStatusFilter(statuses, all)
			if err != nil {
				return err
			}
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
				// The compact default is for eyes, not parsers: scripts get
				// the complete picture unless they filtered explicitly.
				if filter.implicit {
					filter = statusFilter{}
				}
				return printListJSON(cmd, st, chromeStatus, filter)
			}
			return printListTable(cmd, st, chromeStatus, filter, full)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON (always all statuses and fields unless --status is given)")
	cmd.Flags().BoolVar(&share, "share", false, "print install commands for the enabled extensions, ready to paste")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "show every status, one row per extension (default: only what is loaded)")
	cmd.Flags().BoolVar(&full, "full", false, "show all columns (EXTENSION, ID, DIR, URL), one row per extension")
	cmd.Flags().StringSliceVar(&statuses, "status", nil,
		"only list these statuses, one row per extension (comma-separated): "+strings.Join(listStatuses, ", "))
	cmd.MarkFlagsMutuallyExclusive("json", "share")
	// --share answers "what do you run?", a selection by enabledness, not by
	// status or presentation; combining would silently drop the other flag.
	cmd.MarkFlagsMutuallyExclusive("share", "status")
	cmd.MarkFlagsMutuallyExclusive("share", "all")
	cmd.MarkFlagsMutuallyExclusive("share", "full")
	cmd.MarkFlagsMutuallyExclusive("all", "status")
	cmd.MarkFlagsMutuallyExclusive("json", "full")
	return cmd
}

// The status vocabulary: stable single words for --status and the JSON
// statusKey, distinct from the table's advice-bearing display strings.
const (
	statusLoaded         = "loaded"          // enabled and live in Chrome
	statusChromeDisabled = "chrome-disabled" // loaded, switched off in chrome://extensions
	statusNotLoaded      = "not-loaded"      // enabled here, absent from Chrome
	statusNotEnabled     = "not-enabled"     // loaded in Chrome, not enabled here
	statusAvailable      = "available"       // registered, not enabled
	statusUnknown        = "unknown"         // host unreachable, so unknowable
	statusStale          = "stale"           // Chrome-side leftover; cleanup removes it
)

// listStatuses fixes the order used by help text, error messages and the
// not-shown summary.
var listStatuses = []string{statusLoaded, statusChromeDisabled, statusNotLoaded,
	statusNotEnabled, statusAvailable, statusUnknown, statusStale}

// statusFilter selects which statuses list shows. A nil keys map shows
// everything; implicit marks the loaded-only default, which — unlike an
// explicit --status — must account for what it hides.
type statusFilter struct {
	keys     map[string]bool
	implicit bool
}

func (f statusFilter) match(key string) bool { return f.keys == nil || f.keys[key] }

func (f statusFilter) names() []string {
	var ns []string
	for _, s := range listStatuses {
		if f.keys[s] {
			ns = append(ns, s)
		}
	}
	return ns
}

// newStatusFilter turns the flags into a filter. Unknown words fail up front,
// so a typo reads as an error rather than as an empty table.
func newStatusFilter(statuses []string, all bool) (statusFilter, error) {
	if all {
		return statusFilter{}, nil
	}
	if len(statuses) == 0 {
		// The default answers "what is running?". unknown stays in: with the
		// host unreachable it is the honest answer for every enabled
		// extension, and hiding it would make the table empty whenever
		// Chrome is closed.
		return statusFilter{keys: map[string]bool{statusLoaded: true, statusUnknown: true}, implicit: true}, nil
	}
	keys := map[string]bool{}
	for _, v := range statuses {
		v = strings.ToLower(strings.TrimSpace(v))
		if !slices.Contains(listStatuses, v) {
			return statusFilter{}, fmt.Errorf("unknown status %q (valid: %s)",
				term.Safe(v), strings.Join(listStatuses, ", "))
		}
		keys[v] = true
	}
	return statusFilter{keys: keys}, nil
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

// extStatusKey classifies an extension into one word of the vocabulary;
// extStatusText renders that word for the table. Split so that filtering and
// display cannot drift apart.
func extStatusKey(chromeStatus map[string]ipc.ChromeExt, e state.Extension) string {
	if chromeStatus == nil {
		if !e.Enabled() {
			return statusAvailable
		}
		return statusUnknown
	}
	ce, loaded := chromeStatus[e.ID]
	switch {
	case !e.Enabled() && !loaded:
		return statusAvailable
	case !e.Enabled() && loaded:
		return statusNotEnabled
	case !loaded:
		return statusNotLoaded
	case !ce.Enabled:
		return statusChromeDisabled
	default:
		return statusLoaded
	}
}

func extStatusText(key string) string {
	switch key {
	case statusUnknown:
		return "unknown (host not connected)"
	case statusNotEnabled:
		return "loaded but not enabled in cepm — cepm enable?"
	case statusNotLoaded:
		return "NOT LOADED — load it or run: cepm disable"
	case statusChromeDisabled:
		return "loaded (disabled in Chrome)"
	default:
		return key // loaded, available: the key reads as the status
	}
}

const staleStatusText = "stale — run: cepm cleanup"

func trackRef(r *state.Repo) string {
	// Branch and tag names come from the remote; Validate rejects control
	// characters in a state it accepts, but list renders broken states too.
	if r.Track == state.TrackTag {
		return fmt.Sprintf("tag:%s", term.Safe(r.Tag))
	}
	return fmt.Sprintf("branch:%s", term.Safe(r.Branch))
}

func printListTable(cmd *cobra.Command, st *state.State, chromeStatus map[string]ipc.ChromeExt, filter statusFilter, full bool) error {
	out := cmd.OutOrStdout()
	if len(st.Repos) == 0 && len(st.Orphans) == 0 {
		fmt.Fprintln(out, "No repositories registered. Run: cepm install <git-url>")
		return nil
	}
	if len(st.Repos) == 0 {
		fmt.Fprintln(out, "No repositories registered.")
	}
	// The one-row-per-repo summary belongs to the default view only. Asking
	// for statuses (--all, --status) means asking to *see* those extensions —
	// a count in a cell would answer "how many", not "which" — so any
	// explicit selection expands to one row per extension; --full then only
	// adds the identifying columns.
	detail := full || !filter.implicit
	hidden := map[string]int{}
	var rows []string
	for _, name := range st.RepoNames() {
		r := st.Repos[name]
		// The URL is what a colleague needs to install the same extension;
		// last column, so its length does not push the narrow ones apart.
		counts := map[string]int{}
		for _, e := range r.Extensions {
			key := extStatusKey(chromeStatus, e)
			if !filter.match(key) {
				hidden[key]++
				continue
			}
			counts[key]++
			switch {
			case full:
				rows = append(rows, strings.Join([]string{name, trackRef(r), e.Name, e.ID,
					term.Safe(e.Dir), extStatusText(key), term.Safe(gitx.RedactURL(r.URL))}, "\t"))
			case detail:
				rows = append(rows, strings.Join([]string{name, trackRef(r), e.Name, extStatusText(key)}, "\t"))
			}
		}
		// Stale rows are never aggregated: only an explicit selection (--all,
		// --status stale) matches them, and an explicit selection always
		// renders detail rows.
		for _, s := range r.Stale {
			if !filter.match(statusStale) {
				hidden[statusStale]++
				continue
			}
			switch {
			case full:
				rows = append(rows, strings.Join([]string{name, "", s.Name, s.ID,
					"(" + term.Safe(s.Reason) + ")", staleStatusText, ""}, "\t"))
			case detail:
				rows = append(rows, strings.Join([]string{name, "", s.Name, staleStatusText}, "\t"))
			}
		}
		if !detail && len(counts) > 0 {
			rows = append(rows, strings.Join([]string{name, trackRef(r), statusSummary(counts)}, "\t"))
		}
	}
	for _, o := range st.Orphans {
		if !filter.match(statusStale) {
			hidden[statusStale]++
			continue
		}
		switch {
		case full:
			rows = append(rows, strings.Join([]string{"(uninstalled)", "", o.Name, o.ID,
				"(" + term.Safe(o.Reason) + ")", staleStatusText, ""}, "\t"))
		case detail:
			rows = append(rows, strings.Join([]string{"(uninstalled)", "", o.Name, staleStatusText}, "\t"))
		}
	}
	if len(rows) > 0 {
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		switch {
		case full:
			fmt.Fprintln(w, "REPO\tTRACK\tEXTENSION\tID\tDIR\tSTATUS\tURL")
		case detail:
			fmt.Fprintln(w, "REPO\tTRACK\tEXTENSION\tSTATUS")
		default:
			fmt.Fprintln(w, "REPO\tTRACK\tSTATUS")
		}
		for _, r := range rows {
			fmt.Fprintln(w, r)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	} else if filter.implicit {
		fmt.Fprintln(out, "No loaded extensions.")
	} else if filter.keys != nil {
		fmt.Fprintf(out, "No extensions with status %s.\n", strings.Join(filter.names(), ", "))
	} else {
		// --all with repositories but no extension left in any of them
		// (removed upstream, then cleaned up): silence would read like an
		// unregistered state.
		fmt.Fprintln(out, "No extensions registered.")
	}
	// The default view may narrow the table, but never silently: a NOT
	// LOADED row exists to be seen. An explicit --status asked for exactly
	// its subset, so only the implicit filter accounts for the rest.
	if filter.implicit && len(hidden) > 0 {
		if len(rows) > 0 {
			fmt.Fprintln(out)
		}
		hint := "cepm list --all"
		if full {
			hint = "cepm list --all --full" // keep the shape the user chose
		}
		fmt.Fprintf(out, "Not shown: %s. See: %s\n", hiddenSummary(hidden), hint)
	}
	// Errors go below the table: they are multi-line git output, which would
	// otherwise split rows and destroy the column alignment. They show
	// regardless of the filter — a failing update is never a hidden row.
	for _, name := range st.RepoNames() {
		if e := st.Repos[name].LastError; e != "" {
			fmt.Fprintf(out, "\n⚠ %s: last update failed: %s\n", name, term.Safe(oneLine(e)))
		}
	}
	return nil
}

// statusSummary renders a repo's visible rows as one STATUS cell: the full
// advice text while there is a single row to describe, counts of the short
// vocabulary once there are more (the advice does not aggregate). It only
// serves the implicit default view — every explicit selection renders
// detail rows — so its statuses are the default filter's: loaded, unknown.
func statusSummary(counts map[string]int) string {
	total, single := 0, ""
	for key, n := range counts {
		total += n
		single = key
	}
	if total == 1 {
		return extStatusText(single)
	}
	var parts []string
	for _, s := range listStatuses {
		if n := counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	return strings.Join(parts, ", ")
}

func hiddenSummary(hidden map[string]int) string {
	var parts []string
	for _, s := range listStatuses {
		if n := hidden[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	return strings.Join(parts, ", ")
}

func printListJSON(cmd *cobra.Command, st *state.State, chromeStatus map[string]ipc.ChromeExt, filter statusFilter) error {
	type extOut struct {
		Name      string `json:"name"`
		Dir       string `json:"dir"`
		AbsDir    string `json:"absDir"`
		ID        string `json:"id"`
		Enabled   bool   `json:"enabled"`
		Status    string `json:"status"`
		StatusKey string `json:"statusKey"`
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
			key := extStatusKey(chromeStatus, e)
			if !filter.match(key) {
				continue
			}
			ro.Extensions = append(ro.Extensions, extOut{
				Name: e.Name, Dir: e.Dir, AbsDir: filepath.Join(dir, e.Dir),
				ID: e.ID, Enabled: e.Enabled(), Status: extStatusText(key), StatusKey: key,
			})
		}
		if filter.match(statusStale) {
			for _, s := range r.Stale {
				ro.Stale = append(ro.Stale, staleOut{ID: s.ID, Name: s.Name, Reason: s.Reason, NewDir: s.NewDir})
			}
		}
		// A filter selects rows, and a repo left with none is no row; repo
		// metadata alone would read as a match to a script iterating repos.
		// lastError is the exception: like the table's ⚠ line, a failing
		// update is never hidden — a health check filtering on not-loaded
		// must still see that the pulls stopped working.
		if filter.keys != nil && len(ro.Extensions) == 0 && len(ro.Stale) == 0 && ro.LastError == "" {
			continue
		}
		repos = append(repos, ro)
	}
	orphans := []staleOut{}
	if filter.match(statusStale) {
		for _, o := range st.Orphans {
			orphans = append(orphans, staleOut{ID: o.ID, Name: o.Name, Reason: o.Reason, NewDir: o.NewDir})
		}
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"repos": repos, "orphans": orphans})
}
