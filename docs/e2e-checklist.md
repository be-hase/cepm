# Manual E2E checklist

> Most of the end-to-end behavior is now covered automatically by `make e2e`
> (helper connectivity, reload actually changing the running code, updates
> pulled while Chrome was closed, the periodic auto-update, and the helper
> refresh). What remains below is what a machine cannot check: interactive
> prompts, the clipboard, and Chrome's own confirmation dialogs. Sections 1-5
> and 7-8 are largely redundant with `make e2e`; run them when touching that
> area or before a release.

Chrome's real behavior (service-worker lifetime, native messaging launch,
reload-from-disk on setEnabled) can't be covered by unit tests. Run this
checklist before tagging a release, on macOS with a normal Chrome profile.

Preparation: build (`make build`), and use a scratch home so your real setup
is untouched — `export CEPM_HOME=$(mktemp -d)` in every terminal you use.
Note: `cepm setup` still writes the native messaging manifest to the real
`~/Library/Application Support/Google/Chrome/NativeMessagingHosts/`; remove
`com.github.be_hase.cepm.json` afterwards if you don't want to keep it.

## 1. Setup

- [ ] `cepm setup` prints the helper dir and manifest path.
- [ ] chrome://extensions → Developer mode → Load unpacked → `$CEPM_HOME/helper`.
      The ID shown by Chrome equals the one printed by setup
      (`mdnfnogffnkigldddmnmfganbalgaggb`).
- [ ] `cepm doctor`: everything green; "helper ⇄ host" shows a recent
      keep-alive.
- [ ] `~/.cepm/logs/host.log` (under `$CEPM_HOME`) shows "helper connected"
      and "became leader".

## 2. Install and reload

- [ ] `cepm install <test repo url>` detects the expected extensions.
- [ ] Load the printed directories in chrome://extensions.
- [ ] `cepm list` shows STATUS `loaded` for each.
- [ ] Push a commit touching one extension → `cepm update` reports
      `↻ reloaded <name>` and Chrome shows the new behavior immediately.
- [ ] Push a commit touching only one of two extensions → only that one
      reloads.
- [ ] `cepm reload` (no pull) reloads extensions after a local file edit.

## 3. Auto update

- [ ] `echo '[update]\ninterval = "1m"' > $CEPM_HOME/config.toml`, restart
      Chrome (to restart the host), push a commit, wait ≤ ~3 minutes: the
      extension reloads without any CLI interaction (watch host.log).

## 4. Chrome closed

- [ ] Quit Chrome. `cepm update` still pulls and prints "Chrome is not
      running…". Start Chrome: the extension already runs the new version.

## 5. Resilience

- [ ] chrome://serviceworker-internals → stop the cepm helper worker. Within
      ~1 minute doctor shows a fresh keep-alive again (alarm reconnect).
- [ ] Kill the host process (`pkill -f 'cepm.*chrome-extension'`). Chrome
      reconnects (backoff) and `cepm doctor` recovers without user action.
- [ ] Make a clone dirty (`touch` a file inside it) → auto/manual update
      skips it with a warning; `cepm update --force` succeeds and restores
      the local file.

## 6. Selection & lifecycle

- [ ] Install a repo with 2+ extensions on a TTY: the numbered prompt
      appears; Enter enables all; "1" enables only the first.
- [ ] The load ceremony copies the path to the clipboard, opens
      chrome://extensions, and prints "✔ loaded!" within a second of
      clicking Load unpacked. Loading a wrong directory prints the
      mismatch warning.
- [ ] `cepm enable <repo>` lists available extensions interactively;
      after enabling, doctor tracks it until loaded.
- [ ] `cepm disable` on a loaded extension offers Chrome-side removal and
      Chrome shows its confirmation dialog; cancelling is reported.
- [ ] Rename an extension dir in the repo, push, `cepm update`: the move is
      reported, enabled intent carries over, `cepm list` shows a stale row,
      and `cepm cleanup` removes the broken entry via the dialog.
- [ ] Delete an extension dir, push, update: reported as removed; cleanup
      clears the Chrome entry.

## 7. Tag tracking

- [ ] Install a repo with `--track tag`. Push commits to main without a tag:
      `cepm update` stays on the old tag. Push a higher semver tag: update
      checks it out and reloads.

## 8. Upgrade path

- [ ] Bump `helperext.Version`, rebuild, restart Chrome (no `cepm setup`):
      host.log shows "helper files refreshed"; after one more Chrome restart
      chrome://extensions shows the new helper version. This is the
      zero-user-action upgrade path.
- [ ] Change a manifest.json in a managed repo, push, `cepm update`: the
      output warns that a restart is needed, `cepm doctor` flags the version
      mismatch, and restarting Chrome clears it.
- [ ] Move the cepm binary to a new path, run any cepm command (e.g.
      `cepm list`): `~/.cepm/bin/cepm-host` records the new path (self-heal)
      and `cepm doctor` stays green without re-running setup.
- [ ] (mise install) `mise up` to a new cepm version, do NOT run any cepm
      command, restart Chrome: the launcher falls back to the mise shim and
      the host comes up with the new version (check host.log).
