# AGENTS.md

Guidance for AI coding agents working in this repository. This file is the
single source of truth; `CLAUDE.md` just points here.

## What cepm is

A CLI that keeps git-distributed, unpacked Chrome extensions up to date. It
clones repositories into `~/.cepm/repos/<name>`, pulls them (periodically
while Chrome runs, or on demand), and asks a generated helper extension —
over Chrome Native Messaging — to reload the extensions whose files changed.
See `README.md` for the user-facing description and the architecture diagram.

Single binary, Go, macOS and Linux. Windows is deliberately unsupported for
now (`internal/paths` has the OS-specific pieces).

## Commands

```console
make build     # -> bin/cepm (embeds VERSION via ldflags)
make test      # go test ./...
make lint      # gofmt -l check + go vet
make e2e       # real Chrome; downloads Chrome for Testing on first run
```

`make e2e` is not optional for changes touching the host, the helper, the
IPC protocol, or reload behaviour — it is the only thing that exercises a
real Chrome. `CEPM_E2E_HEADED=1` to watch it.

Run `go test ./... -race` before finishing anything with concurrency in it.

## The shape of the system

- `cmd/cepm` — entry point, nothing but wiring.
- `internal/cli` — one file per command. Commands validate, then delegate.
- `internal/state` — `~/.cepm/state.json`: what is registered, which
  extension ids belong to it, what Chrome still needs cleaning up. Read the
  package doc before changing anything here; it carries the trust model.
- `internal/updater` — the one pull/scan/attribute path, shared by the CLI
  and the host. Owns the update lock.
- `internal/nmhost` — the native messaging host Chrome launches, the leader
  election, the periodic updater, the control socket server.
- `internal/ipc` — CLI ⇄ host over a unix socket. `internal/helperext` — the
  generated MV3 helper (embedded JS) and its fixed extension id.
- `internal/extid` — extension ids: SHA-256 of the manifest `key` (when
  present) or of the absolute path, nibble → `a`..`p`. Everything that
  identifies an extension goes through here.
- `internal/term` — everything that reaches a terminal goes through `Safe`,
  `Strip` or `Quote`.

## Invariants worth knowing before you edit

These were each learned from a real defect. Breaking one usually looks fine
in a quick test.

**One writer at a time.** Every `state.json` write happens inside
`updater.WithLock`. So does anything that decides from the state and then
acts on Chrome — reads and side effects must not be split by a window
another process can write in. The exception is Chrome-side *removal*: the
CLI already holds the lock while sending it, so the host authorizes those
without taking it (taking it would deadlock). The reload paths all funnel
through `Host.reloadEnabled` for exactly this reason.

**State before filesystem.** Delete or move a clone only after the state
naming it is safely on disk. The reverse order leaves a repository
registered with nothing behind it, which nothing can repair.

**Ask outside the lock, act inside it.** Prompts wait for a human, which the
lock must not. Collect the answer first, re-check the state under the lock,
and only then touch Chrome — so "nothing was changed" is true when cepm
aborts.

**`Repo.Head` means "fully processed".** It advances only past a commit that
was scanned and saved. A clone ahead of it is a previous run that failed
partway; the next update reprocesses that range rather than calling it "no
change".

**The state is trusted, and validated anyway.** `state.Validate` is an
integrity check (corruption, half-writes, a moved `~/.cepm`, values that a
terminal or a command line would misread) — not an authorization boundary
against the user, who owns the file. Do not describe it as one. Every field
added to the persisted shape raises `state.Version`; there are no
migrations, and `Load` refuses any other version.

**Nothing untrusted reaches the terminal raw.** Git and SSH stderr, manifest
names, branch names: `term.Safe` on display, `term.Quote` for anything a
user might paste into a shell.

**Chrome reloads code, not manifests.** `setEnabled(false)`→`(true)` re-reads
the code from disk but keeps the cached manifest. When `manifest.json`
changed, say so — do not pretend the update is fully live.

## How to work here

**Verify, don't assert.** Reproduce a bug in code (or with real `git`/`sh`)
before fixing it, and prove the fix with a test.

**Mutation-check every fix.** After the test passes, deliberately break the
fix again and confirm the suite fails. This repeatedly caught tests that
passed against broken code — a test that never fails is worse than no test,
because it reads as coverage. Report honestly when a mutation goes
undetected instead of quietly moving on.

**No timing-based synchronization in tests.** A `time.Sleep` to let another
goroutine "get far enough" fails on a loaded machine. Synchronize on
channels or an injected hook. Polling until a condition holds, with a
deadline, is fine — slow machines just iterate more.

**Tests must not touch the developer's real world.** No writes outside
`t.TempDir()`/`CEPM_HOME`, no opening the user's Chrome, no clipboard. Side
effects go through variables that `TestMain` stubs (see
`internal/cli/main_test.go`).

**Comments say why, not what.** A comment earns its place by recording a
constraint the code cannot show — why this order, which failure this
prevents. Do not narrate the next line or address the reviewer.

**Errors are for the person stuck.** Say what happened and what to do next,
name the path or command, and never echo a URL that may carry a token
(`gitx.RedactURL`).

## Conventions

- Go 1.25 (mise pins the toolchain). Dependencies: cobra, go-toml/v2, flock,
  `golang.org/x/mod/semver`. git is invoked as a subprocess, deliberately, so
  the user's SSH config and credential helpers just work.
- `gofmt` clean, `go vet` clean; both are CI gates.
- Commit messages: a one-line summary, then what changed and why. Japanese is
  fine (existing history is Japanese).
- Commit as work progresses rather than one large drop.
