// Package gitx wraps the git CLI. It deliberately shells out to git instead of
// using go-git so that the user's SSH config, credential helpers, and proxy
// settings work unchanged with internal git servers.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"regexp"
	"strings"

	"github.com/be-hase/cepm/internal/term"
)

// Repo is a git working tree at Dir.
type Repo struct {
	Dir string
}

// urlRe finds anything URL-shaped in a longer message so it can be redacted
// even when embedded in git's output.
var urlRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"]+`)

// RedactURL removes the secret-bearing parts of every URL in s, so tokens do
// not reach the terminal, the logs, or a pasted bug report. The userinfo is
// stripped via net/url (which also catches a bare token with no password
// separator, unlike a "user:pass@" pattern); the query and fragment are
// replaced wholesale, because token parameter names are too varied to
// enumerate. Scheme, host and path survive — they are what a diagnosis
// needs. Display-only: what git is invoked with and what state.json stores
// are never touched.
func RedactURL(s string) string {
	return urlRe.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			// Fail closed: an unparsable URL (a stray %-escape is enough)
			// cannot be split into safe and secret parts, and its userinfo
			// or query may carry a token — losing the diagnosis beats
			// printing it.
			return "***"
		}
		hadUser := u.User != nil
		u.User = nil
		if u.RawQuery != "" {
			u.RawQuery = "***"
		}
		if u.Fragment != "" {
			u.Fragment = "***"
			u.RawFragment = ""
		}
		out := u.String()
		if hadUser {
			// Re-insert a literal marker: passing "***" through url.User
			// would percent-encode it.
			out = strings.Replace(out, "://", "://***@", 1)
		}
		return out
	})
}

func run(ctx context.Context, dir string, args ...string) (string, error) {
	// core.quotePath would C-escape any path containing non-ASCII bytes
	// ("ext/\343\202\242..."), which no caller un-escapes, silently breaking
	// change attribution for such extensions.
	cmdArgs := append([]string{"-c", "core.quotePath=false"}, args...)
	if dir != "" {
		cmdArgs = append([]string{"-C", dir}, cmdArgs...)
	}
	slog.Debug("git", "args", RedactURL(strings.Join(args, " ")), "dir", dir)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// Redact: args and git's own output can contain a URL with embedded
		// credentials, and this text is shown, logged, and pasted into chats.
		// Escape: stderr is written by the remote ("remote:" lines, SSH
		// banners), so it can carry escape sequences aimed at the terminal.
		return "", fmt.Errorf("git %s: %s",
			term.SafeLines(RedactURL(strings.Join(args, " "))),
			term.SafeLines(RedactURL(msg)))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Clone clones url into dir. branch may be empty (remote default branch).
func Clone(ctx context.Context, url, dir, branch string) error {
	args := []string{"clone"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", url, dir)
	_, err := run(ctx, "", args...)
	return err
}

// Version returns the installed git version string, or an error if git is
// missing from PATH.
func Version(ctx context.Context) (string, error) {
	return run(ctx, "", "version")
}

func (r Repo) Head(ctx context.Context) (string, error) {
	return run(ctx, r.Dir, "rev-parse", "HEAD")
}

// CurrentBranch returns the checked-out branch name, or "" when HEAD is
// detached.
func (r Repo) CurrentBranch(ctx context.Context) (string, error) {
	out, err := run(ctx, r.Dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if out == "HEAD" {
		return "", nil
	}
	return out, nil
}

func (r Repo) IsDirty(ctx context.Context) (bool, error) {
	out, err := run(ctx, r.Dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// Fetch fetches the remote, including tags and pruning deleted refs.
//
// --force and --prune-tags matter for repositories that move release tags:
// without them a re-pushed tag makes fetch exit non-zero forever ("would
// clobber existing tag"), which would block every later update of that repo,
// and a tag deleted upstream would linger and keep being followed.
func (r Repo) Fetch(ctx context.Context) error {
	_, err := run(ctx, r.Dir, "fetch", "--tags", "--prune", "--prune-tags", "--force", "origin")
	return err
}

// MergeFFOnly fast-forwards the current branch to ref, failing if a
// fast-forward is not possible (e.g. rewritten history).
func (r Repo) MergeFFOnly(ctx context.Context, ref string) error {
	_, err := run(ctx, r.Dir, "merge", "--ff-only", ref)
	return err
}

// TagsByCreation returns tags matching the glob pattern, most recently
// created first. The pattern comes from the repository, so it is passed
// after "--" and callers validate it (see scan.ValidTagPattern).
func (r Repo) TagsByCreation(ctx context.Context, pattern string) ([]string, error) {
	out, err := run(ctx, r.Dir, "tag", "--sort=-creatordate", "--list", "--", pattern)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// CommitOf resolves a ref (tag, branch, HEAD) to its commit id.
func (r Repo) CommitOf(ctx context.Context, ref string) (string, error) {
	return run(ctx, r.Dir, "rev-parse", ref+"^{commit}")
}

// CheckoutDetached checks out ref as a detached HEAD.
func (r Repo) CheckoutDetached(ctx context.Context, ref string) error {
	_, err := run(ctx, r.Dir, "checkout", "--detach", "--quiet", ref)
	return err
}

// ChangedFiles lists paths (repo-relative) that differ between two commits.
// The trailing "--" terminates the revision list: the callers validate that
// from/to are commit OIDs, and this makes git agree that nothing here is an
// option or a pathspec.
func (r Repo) ChangedFiles(ctx context.Context, from, to string) ([]string, error) {
	out, err := run(ctx, r.Dir, "diff", "--name-only", from, to, "--")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// StashPush stashes local changes including untracked files and returns the
// commit id of the entry it created, or "" when it created none: "git status
// --porcelain" considers e.g. a dirty submodule a change, but stash refuses
// to save it ("No local changes to save") and still exits 0. Popping in that
// case would drop an unrelated stash entry belonging to the user.
//
// The id is what StashPop restores by: stash positions shift, and the entry
// cepm pushed is not necessarily the top one by the time it pops.
func (r Repo) StashPush(ctx context.Context) (stashID string, err error) {
	before, err := r.stashTop(ctx)
	if err != nil {
		return "", err
	}
	if _, err := run(ctx, r.Dir, "stash", "push", "--include-untracked", "--message", "cepm auto-stash"); err != nil {
		return "", err
	}
	after, err := r.stashTop(ctx)
	if err != nil {
		return "", err
	}
	if after == "" || after == before {
		return "", nil
	}
	return after, nil
}

// StashRef returns the stash reference (stash@{n}) currently holding the
// entry with the given commit id, or "" when the entry is gone.
func (r Repo) StashRef(ctx context.Context, stashID string) (string, error) {
	out, err := run(ctx, r.Dir, "stash", "list", "--format=%H %gd")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		id, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && id == stashID {
			return ref, nil
		}
	}
	return "", nil
}

// stashTop returns the commit id of refs/stash, or "" when there is none.
func (r Repo) stashTop(ctx context.Context) (string, error) {
	out, err := run(ctx, r.Dir, "rev-parse", "-q", "--verify", "refs/stash")
	if err != nil {
		// No stash exists: rev-parse --verify exits non-zero, which is not a
		// failure for us.
		return "", nil
	}
	return out, nil
}

// StashPop restores the entry StashPush created, by id — never "the top
// one". The user (or another tool) can push a stash in the clone while cepm
// is pulling, and popping blind would apply their work, delete their entry,
// and leave cepm's own changes sitting in the stash. On conflict git keeps
// the entry; callers should surface the error to the user.
func (r Repo) StashPop(ctx context.Context, stashID string) error {
	ref, err := r.StashRef(ctx, stashID)
	if err != nil {
		return err
	}
	if ref == "" {
		return fmt.Errorf("the auto-stash %s is no longer in the stash list", stashID)
	}
	_, err = run(ctx, r.Dir, "stash", "pop", ref)
	return err
}
