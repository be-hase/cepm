package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/term"
	"github.com/be-hase/cepm/internal/updater"
)

// renameForBackup is a seam for tests that need one of the moves to fail:
// both directories involved are owned by this user, so there is no way to
// provoke that through the filesystem.
var renameForBackup = os.Rename

// newResetCmd moves the state and the clones out of the way so cepm can
// start over. It exists for the one situation cepm refuses to handle
// automatically: a state file it cannot use (corrupted, hand-edited, or from
// an incompatible build). Deleting the file alone would strand the clones —
// install refuses an occupied directory, and uninstall no longer knows the
// name — and would silently lose the repo URLs and Chrome bookkeeping, so
// everything is renamed into a backup instead. Nothing is deleted.
func newResetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Move an unusable state and all clones to a backup and start over",
		Long: `Reset moves state.json and the cloned repositories into a time-stamped
backup directory under ~/.cepm. Use it when cepm reports that it cannot use
the state file. Nothing is deleted: the repo URLs can be recovered from the
backup, and the clones stay intact until you remove the backup yourself.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			home, err := paths.CepmDir()
			if err != nil {
				return err
			}
			stateFile, err := paths.StateFile()
			if err != nil {
				return err
			}
			reposDir, err := paths.ReposDir()
			if err != nil {
				return err
			}

			var (
				backup        string
				moved         [][2]string
				stateReadable bool
				reposMoved    bool
			)
			// The same lock every state writer holds. A reset racing an
			// update would move the clones the update is working on into the
			// backup, and the update's final save would then recreate a
			// state pointing at them there.
			err = updater.WithLock(cmd.Context(), func() error {
				var sources []string
				for _, src := range []string{stateFile, reposDir} {
					if _, err := os.Lstat(src); err != nil {
						if errors.Is(err, fs.ErrNotExist) {
							continue
						}
						return err // unreadable is not the same as absent
					}
					// WithLock's EnsureLayout recreates repos/ empty on
					// every lock acquisition, so an empty directory means
					// "no clones", not something worth backing up.
					if src == reposDir {
						if entries, err := os.ReadDir(src); err == nil && len(entries) == 0 {
							continue
						}
					}
					sources = append(sources, src)
				}
				if len(sources) == 0 {
					return nil
				}

				// Whether the state can still name the repositories decides
				// the recovery guidance printed below. Parsing alone is not
				// enough: {"repos":{"a":null}} parses fine and holds no URL,
				// and pointing at that file would hide the working recovery
				// route. Only a state where every repository actually
				// carries its URL answers the question being asked.
				if raw, err := os.ReadFile(stateFile); err == nil {
					var probe struct {
						Repos map[string]struct {
							URL string `json:"url"`
						} `json:"repos"`
					}
					if json.Unmarshal(raw, &probe) == nil && len(probe.Repos) > 0 {
						stateReadable = true
						for _, r := range probe.Repos {
							if r.URL == "" {
								stateReadable = false
								break
							}
						}
					}
				}

				// Exclusively created, so a reset never merges into (or
				// overwrites) an earlier backup from the same second.
				backup, err = os.MkdirTemp(home, "backup-"+time.Now().Format("20060102-150405")+"-")
				if err != nil {
					return err
				}
				for _, src := range sources {
					dst := filepath.Join(backup, filepath.Base(src))
					if err := renameForBackup(src, dst); err != nil {
						// All or nothing: a half-moved world — metadata in
						// the backup, clones still active — is exactly the
						// split state reset exists to resolve.
						for _, m := range moved {
							if rbErr := os.Rename(m[1], m[0]); rbErr != nil {
								return fmt.Errorf("move %s to the backup: %w; restoring %s failed too: %v — put it back from %s manually",
									src, err, m[0], rbErr, m[1])
							}
						}
						moved = nil
						_ = os.Remove(backup)
						backup = ""
						return fmt.Errorf("move %s to the backup: %w (everything was put back)", src, err)
					}
					moved = append(moved, [2]string{src, dst})
					if src == reposDir {
						reposMoved = true
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if len(moved) == 0 {
				fmt.Fprintln(out, "Nothing to reset: no state file and no clones.")
				return nil
			}

			for _, m := range moved {
				fmt.Fprintf(out, "moved %s → %s\n", m[0], m[1])
			}
			fmt.Fprintf(out, "\nTo start over:\n")
			switch {
			case stateReadable:
				fmt.Fprintf(out, "  • the repository URLs are in %s\n", term.Quote(filepath.Join(backup, "state.json")))
			case reposMoved:
				fmt.Fprintf(out, "  • the old state file was missing or unreadable, but each clone still knows its URL:\n")
				fmt.Fprintf(out, "      git -C %s remote get-url origin\n", term.Quote(filepath.Join(backup, "repos", "<name>")))
			}
			fmt.Fprintf(out, "  • re-run cepm install <url> for each repository you use\n")
			fmt.Fprintf(out, "  • entries may remain in chrome://extensions — remove the ones you no longer want\n")
			fmt.Fprintf(out, "  • once everything works, the backup can be deleted\n")
			return nil
		},
	}
}
