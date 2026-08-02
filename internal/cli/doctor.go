package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/gitx"
	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/launcher"
	"github.com/be-hase/cepm/internal/nmmanifest"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/scan"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
	"github.com/be-hase/cepm/internal/updater"
)

type diagnostic struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

func newDoctorCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "doctor",
		Short:   "Diagnose the cepm setup and Chrome connectivity",
		GroupID: "setup",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			diags := runDiagnostics(cmd.Context())
			failed := 0
			for _, d := range diags {
				if d.Status == "fail" {
					failed++
				}
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(diags); err != nil {
					return err
				}
			} else {
				out := cmd.OutOrStdout()
				w := tabwriter.NewWriter(out, 2, 4, 1, ' ', 0)
				for _, d := range diags {
					symbol := map[string]string{"ok": "✔", "warn": "⚠", "fail": "✘"}[d.Status]
					fmt.Fprintf(w, " %s\t%s\t%s\n", symbol, d.Name, oneLine(d.Detail))
					if d.Hint != "" {
						fmt.Fprintf(w, " \t\t→ %s\n", oneLine(d.Hint))
					}
				}
				if err := w.Flush(); err != nil {
					return err
				}
			}
			// A non-zero status makes doctor usable as a check in scripts,
			// which is how setup tells people to verify their install.
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func runDiagnostics(ctx context.Context) []diagnostic {
	var diags []diagnostic
	add := func(name, status, detail, hint string) {
		diags = append(diags, diagnostic{Name: name, Status: status, Detail: detail, Hint: hint})
	}

	add("cepm binary", "ok", fmt.Sprintf("%s (%s)", executablePath(), Version), "")

	if v, err := gitx.Version(ctx); err != nil {
		add("git", "fail", err.Error(), "install git and make sure it is on PATH")
	} else {
		add("git", "ok", v, "")
	}

	if dir, err := paths.CepmDir(); err != nil {
		add("cepm home", "fail", err.Error(), "")
	} else if _, err := os.Stat(dir); err != nil {
		add("cepm home", "fail", dir+" does not exist", "run: cepm setup")
	} else {
		add("cepm home", "ok", dir, "")
	}

	diags = append(diags, checkHelperFiles()...)
	diags = append(diags, checkLauncher()...)
	diags = append(diags, checkNMManifests()...)

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	info, pingErr := ipc.Ping(pingCtx)
	if pingErr != nil {
		add("host process", "warn", "not reachable",
			"Chrome may not be running, or the helper extension is not loaded/enabled (chrome://extensions). Updates still apply on next Chrome launch.")
	} else {
		add("host process", "ok",
			fmt.Sprintf("connected (pid %d, leader=%t, up %s)",
				info.PID, info.Leader, time.Since(info.StartedAt).Round(time.Second)), "")
		switch {
		case info.HelperVersion == "":
			add("helper ⇄ host", "warn", "helper has not said hello yet", "check chrome://extensions for the cepm helper")
		case info.LastPong.IsZero():
			// Keep-alives start one interval after the connection, so this is
			// normal right after Chrome launches.
			add("helper ⇄ host", "ok",
				fmt.Sprintf("helper v%s connected %s ago (no keep-alive exchanged yet)",
					info.HelperVersion, time.Since(info.StartedAt).Round(time.Second)), "")
		case time.Since(info.LastPong) > 2*time.Minute:
			add("helper ⇄ host", "warn",
				fmt.Sprintf("last keep-alive from helper was %s ago", time.Since(info.LastPong).Round(time.Second)),
				"the helper's service worker may be stuck; toggle the helper in chrome://extensions")
		default:
			add("helper ⇄ host", "ok",
				fmt.Sprintf("helper v%s, keep-alive %s ago",
					info.HelperVersion, time.Since(info.LastPong).Round(time.Second)), "")
		}
	}

	diags = append(diags, checkRepos(ctx, pingErr == nil)...)
	// Deliberately no check for leftover .install-* staging directories: an
	// install in progress owns one, and doctor cannot tell that apart from
	// an abandoned one (install holds no lock while it clones and prompts).
	// Calling a live install's clone "safe to delete" would be worse than
	// leaving a few megabytes around; an incomplete install at the final
	// path is diagnosed by install itself.
	return diags
}

func checkHelperFiles() []diagnostic {
	helperDir, err := paths.HelperDir()
	if err != nil {
		return []diagnostic{{Name: "helper extension", Status: "fail", Detail: err.Error()}}
	}
	// The version marker alone is not evidence: check the files it describes.
	for _, f := range []string{"manifest.json", "background.js"} {
		if st, err := os.Stat(filepath.Join(helperDir, f)); err != nil || st.Size() == 0 {
			return []diagnostic{{Name: "helper extension", Status: "fail",
				Detail: fmt.Sprintf("%s is missing or empty", filepath.Join(helperDir, f)),
				Hint:   "run: cepm setup --force"}}
		}
	}
	switch v := helperext.InstalledVersion(helperDir); {
	case v == "":
		return []diagnostic{{Name: "helper extension", Status: "fail",
			Detail: "not generated", Hint: "run: cepm setup"}}
	case v != helperext.Version:
		return []diagnostic{{Name: "helper extension", Status: "warn",
			Detail: fmt.Sprintf("v%s installed, v%s embedded in this binary", v, helperext.Version),
			Hint:   "run: cepm setup"}}
	default:
		return []diagnostic{{Name: "helper extension", Status: "ok",
			Detail: fmt.Sprintf("%s (v%s, id %s)", helperDir, v, helperext.ExtensionID())}}
	}
}

// checkLauncher verifies the host launcher script the NM manifest points at.
// SelfHeal runs on every cepm invocation, so a mismatch here means healing
// itself failed (e.g. permissions).
func checkLauncher() []diagnostic {
	launcherPath, err := paths.LauncherPath()
	if err != nil {
		return []diagnostic{{Name: "host launcher", Status: "fail", Detail: err.Error()}}
	}
	recorded, err := launcher.RecordedPath()
	if err != nil {
		return []diagnostic{{Name: "host launcher", Status: "fail",
			Detail: err.Error(), Hint: "run: cepm setup"}}
	}
	switch {
	case recorded == "":
		return []diagnostic{{Name: "host launcher", Status: "fail",
			Detail: "not installed", Hint: "run: cepm setup"}}
	case recorded != executablePath():
		return []diagnostic{{Name: "host launcher", Status: "warn",
			Detail: fmt.Sprintf("records %s but cepm now runs from %s", recorded, executablePath()),
			Hint:   "run: cepm setup (self-heal seems unable to write the launcher)"}}
	default:
		if _, err := os.Stat(recorded); err != nil {
			return []diagnostic{{Name: "host launcher", Status: "warn",
				Detail: fmt.Sprintf("recorded binary %s does not exist", recorded),
				Hint:   "run: cepm setup"}}
		}
		return []diagnostic{{Name: "host launcher", Status: "ok",
			Detail: fmt.Sprintf("%s → %s", launcherPath, recorded)}}
	}
}

func checkNMManifests() []diagnostic {
	var diags []diagnostic
	var foundIn []string
	launcherPath, _ := paths.LauncherPath()
	for _, variant := range paths.ChromeVariants {
		m, path, err := nmmanifest.Read(variant)
		if errors.Is(err, fs.ErrNotExist) {
			continue // variant not configured (or not installed); fine
		}
		name := "NM manifest (" + variant + ")"
		if err != nil {
			// A manifest that exists but cannot be read is a broken install,
			// not a missing one: reporting "not installed" would misdiagnose
			// it (the hint still repairs it).
			foundIn = append(foundIn, variant)
			diags = append(diags, diagnostic{Name: name, Status: "fail",
				Detail: oneLine(err.Error()), Hint: "run: cepm setup"})
			continue
		}
		foundIn = append(foundIn, variant)
		// allowed_origins is the authorization boundary for an extension that
		// can disable any other one, so require it to name exactly the cepm
		// helper — an extra entry would be someone else's grant.
		wantOrigin := "chrome-extension://" + helperext.ExtensionID() + "/"
		originOK := len(m.AllowedOrigins) == 1 && m.AllowedOrigins[0] == wantOrigin
		switch {
		case !originOK:
			diags = append(diags, diagnostic{Name: name, Status: "fail",
				Detail: fmt.Sprintf("allowed_origins should list only the cepm helper, but is %v", m.AllowedOrigins),
				Hint:   "run: cepm setup"})
		case m.Path != launcherPath:
			diags = append(diags, diagnostic{Name: name, Status: "warn",
				Detail: fmt.Sprintf("points to %s instead of the launcher %s", m.Path, launcherPath),
				Hint:   "run: cepm setup"})
		default:
			diags = append(diags, diagnostic{Name: name, Status: "ok", Detail: path})
		}
	}
	if len(foundIn) == 0 {
		diags = append(diags, diagnostic{Name: "NM manifest", Status: "fail",
			Detail: "no native messaging host manifest installed", Hint: "run: cepm setup"})
	}
	if len(foundIn) > 1 {
		// Two Chromes could connect, and only one would receive reloads.
		diags = append(diags, diagnostic{Name: "NM manifest", Status: "warn",
			Detail: fmt.Sprintf("registered for %d Chromes (%s); cepm drives exactly one", len(foundIn), strings.Join(foundIn, ", ")),
			Hint:   "run: cepm setup --chrome-variant <the one you use> (it removes the others)"})
	}
	return diags
}

func checkRepos(ctx context.Context, hostReachable bool) []diagnostic {
	var diags []diagnostic
	st, err := state.Load()
	if err != nil {
		return []diagnostic{{Name: "state", Status: "fail", Detail: err.Error(),
			Hint: "run cepm reset to move the state and clones to a backup, then re-install"}}
	}
	if err := st.Validate(); err != nil {
		// Covers both a duplicated id and a directory name cepm would never
		// register; no cepm writes such a file, so it is not repaired —
		// every state-changing command refuses it.
		diags = append(diags, diagnostic{
			Name:   "state",
			Status: "fail",
			Detail: oneLine(err.Error()),
			Hint:   "run cepm reset to move the state and clones to a backup, then re-install",
		})
	}
	if len(st.Orphans) > 0 {
		diags = append(diags, diagnostic{
			Name:   "leftover entries",
			Status: "warn",
			Detail: fmt.Sprintf("%d extension(s) from uninstalled repositories may still be in Chrome", len(st.Orphans)),
			Hint:   "run: cepm cleanup",
		})
	}
	if len(st.Repos) == 0 {
		return append(diags, diagnostic{Name: "repositories", Status: "ok",
			Detail: "none registered", Hint: "add one with: cepm install <git-url>"})
	}
	chromeStatus := map[string]ipc.ChromeExt{}
	if hostReachable {
		if exts, err := ipc.ListChrome(ctx); err == nil {
			for _, e := range exts {
				chromeStatus[e.ID] = e
			}
		} else {
			hostReachable = false
		}
	}
	for _, name := range st.RepoNames() {
		r := st.Repos[name]
		dir, _ := updater.RepoDir(name)
		repoDiag := diagnostic{Name: "repo " + name, Status: "ok",
			Detail: fmt.Sprintf("%s (%s)", dir, trackRef(r))}
		if dirty, err := (gitx.Repo{Dir: dir}).IsDirty(ctx); err != nil {
			repoDiag = diagnostic{Name: "repo " + name, Status: "fail", Detail: err.Error(),
				Hint: fmt.Sprintf("clone may be broken; try: cepm uninstall %s && cepm install %s",
					term.Quote(name), term.Quote(gitx.RedactURL(r.URL)))}
		} else if dirty {
			repoDiag = diagnostic{Name: "repo " + name, Status: "warn",
				Detail: "working tree has local changes — auto update will skip this repo",
				Hint:   fmt.Sprintf("commit/stash them, or use: cepm update --force %s", name)}
		} else if r.LastError != "" {
			repoDiag = diagnostic{Name: "repo " + name, Status: "warn",
				Detail: "last update failed: " + r.LastError}
		}
		diags = append(diags, repoDiag)

		if !hostReachable && len(r.Extensions) > 0 {
			// Say so rather than printing nothing, which reads as "all good".
			diags = append(diags, diagnostic{
				Name:   "repo " + name + " extensions",
				Status: "warn",
				Detail: fmt.Sprintf("cannot check whether %d extension(s) are loaded (host not reachable)", len(r.Extensions)),
			})
		}
		for _, e := range r.Extensions {
			if !hostReachable {
				continue // cannot tell whether extensions are loaded
			}
			ce, loaded := chromeStatus[e.ID]
			if e.Enabled() && loaded {
				if onDisk, err := scan.ManifestVersion(filepath.Join(dir, e.Dir)); err == nil &&
					onDisk != "" && ce.Version != "" && onDisk != ce.Version {
					diags = append(diags, diagnostic{
						Name:   "extension " + e.Name,
						Status: "warn",
						Detail: fmt.Sprintf("manifest.json on disk is %s but Chrome still runs %s", onDisk, ce.Version),
						Hint:   "code changes are live; restart Chrome (or click Reload in chrome://extensions) to apply manifest changes",
					})
				}
			}
			switch {
			case e.Enabled() && !loaded:
				diags = append(diags, diagnostic{
					Name:   "extension " + e.Name,
					Status: "fail",
					Detail: "enabled but not loaded in Chrome",
					Hint: fmt.Sprintf("one-time step: Load unpacked %s (or opt out with: cepm disable %s)",
						term.Quote(filepath.Join(dir, e.Dir)), term.Quote(name+"/"+e.Dir)),
				})
			case e.Enabled() && loaded && !ce.Enabled:
				// Updates respect the Chrome-side switch ("turned off in
				// Chrome; left as is"), so this is the one state where pulls
				// happen but reloads never do — without a diagnostic it reads
				// as "updates don't work".
				diags = append(diags, diagnostic{
					Name:   "extension " + e.Name,
					Status: "warn",
					Detail: "turned off in chrome://extensions — updates pull but reloads are skipped",
					Hint: fmt.Sprintf("turn it on in chrome://extensions, or stop managing it with: cepm disable %s",
						term.Quote(name+"/"+e.Dir)),
				})
			case !e.Enabled() && loaded:
				diags = append(diags, diagnostic{
					Name:   "extension " + e.Name,
					Status: "warn",
					Detail: "loaded in Chrome but not enabled in cepm — updates will NOT reload it",
					Hint:   fmt.Sprintf("run: cepm enable %s", term.Quote(name+"/"+e.Dir)),
				})
			}
		}
		for _, s := range r.Stale {
			diags = append(diags, diagnostic{
				Name:   "stale entry " + s.Name,
				Status: "warn",
				Detail: fmt.Sprintf("extension dir was %s in the repo; its Chrome entry is broken", s.Reason),
				Hint:   "run: cepm cleanup",
			})
		}
	}
	return diags
}

// oneLine collapses embedded newlines so that a multi-line git error (which
// is what LastError usually holds) cannot break column alignment.
// oneLine flattens s for a single table cell. term.Safe last: much of what
// passes through here (git stderr, stale reasons) originates outside cepm,
// and Fields only folds whitespace — ESC sequences and bidi overrides would
// survive it.
func oneLine(s string) string {
	return term.Safe(strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " "))
}

func executablePath() string {
	p, err := os.Executable()
	if err != nil {
		return "(unknown)"
	}
	return p
}
