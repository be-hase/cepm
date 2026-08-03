// Package gitx wraps the git CLI. It deliberately shells out to git instead of
// using go-git so that the user's SSH config, credential helpers, and proxy
// settings work unchanged with internal git servers.
package gitx

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

// runRaw is run without the TrimSpace on stdout, for output where leading
// or trailing blanks are data: porcelain status columns, NUL-separated file
// lists whose names can start with a space.
func runRaw(ctx context.Context, dir string, args ...string) (string, error) {
	cmdArgs := append([]string{"-c", "core.quotePath=false"}, args...)
	if dir != "" {
		cmdArgs = append([]string{"-C", dir}, cmdArgs...)
	}
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s",
			term.SafeLines(RedactURL(strings.Join(args, " "))),
			term.SafeLines(RedactURL(msg)))
	}
	return stdout.String(), nil
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

// ChangeFingerprint identifies the tree's uncommitted state by content,
// returning "" for a clean tree. For callers that ask a person before
// destroying local changes: the answer covers the changes as they were
// then, and only a content-level comparison can tell those from what
// arrived while the question was open — "git status" alone shows the same
// " M file" line no matter how many times the file was rewritten since.
//
// Tracked changes are covered by the diff against HEAD, with
// --submodule=diff so a rewrite inside a submodule changes the diff text
// rather than hiding behind an unchanged "-dirty" line. Untracked entries
// do not appear in any diff, so they are recorded directly: type, mode and
// symlink target from Lstat, bytes for regular files, every field length-
// prefixed so contents cannot shift across record boundaries and compare
// equal. Submodules are fingerprinted recursively, because their untracked
// files appear in no diff or listing of the parent. The diff representation
// is pinned with --no-ext-diff/--no-textconv: an external driver decides
// what the diff *shows*, and this needs what the tree *is*.
//
// An error means the state could not be identified — a caller about to
// destroy the tree must treat that as "do not", not as "nothing to lose":
// the tree that cannot be fingerprinted can still hold the user's work.
func (r Repo) ChangeFingerprint(ctx context.Context) (string, error) {
	// Raw output, not run(): TrimSpace would eat the leading space of the
	// first porcelain status column and of an untracked name that starts
	// with a blank, making two different states print the same.
	porcelain, err := runRaw(ctx, r.Dir, "status", "--porcelain", "-uall", "--ignore-submodules=none")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(porcelain) == "" {
		return "", nil
	}
	var diff string
	if _, herr := run(ctx, r.Dir, "rev-parse", "--verify", "HEAD"); herr == nil {
		diff, err = runRaw(ctx, r.Dir, "diff", "--no-ext-diff", "--no-textconv", "--full-index", "--ignore-submodules=none", "--submodule=diff", "HEAD", "--")
		if err != nil {
			return "", err
		}
	} else {
		// No commits yet (a clone broken mid-setup): the empty-tree-vs-
		// index and index-vs-worktree diffs together cover the same ground.
		staged, serr := runRaw(ctx, r.Dir, "diff", "--no-ext-diff", "--no-textconv", "--full-index", "--ignore-submodules=none", "--cached", "--")
		if serr != nil {
			return "", serr
		}
		unstaged, uerr := runRaw(ctx, r.Dir, "diff", "--no-ext-diff", "--no-textconv", "--full-index", "--ignore-submodules=none", "--")
		if uerr != nil {
			return "", uerr
		}
		diff = staged + "\x00" + unstaged
	}
	untracked, err := runRaw(ctx, r.Dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	record := func(field string) {
		fmt.Fprintf(h, "%d:", len(field))
		h.Write([]byte(field))
	}
	record(porcelain)
	record(diff)
	record(untracked)
	for _, p := range strings.Split(untracked, "\x00") {
		if p == "" {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		full := filepath.Join(r.Dir, p)
		fi, err := os.Lstat(full)
		if err != nil {
			return "", fmt.Errorf("fingerprint %s: %w", p, err)
		}
		record(p)
		record(fi.Mode().String())
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			// Readlink, not ReadFile: the link itself is the entry, and a
			// dangling target is a perfectly healthy thing to have lying
			// around — it must neither fail the fingerprint nor read as
			// whatever it happens to point at.
			target, err := os.Readlink(full)
			if err != nil {
				return "", fmt.Errorf("fingerprint %s: %w", p, err)
			}
			record(target)
		case fi.Mode().IsRegular():
			if err := hashFile(h, full, fi.Size()); err != nil {
				return "", fmt.Errorf("fingerprint %s: %w", p, err)
			}
		case fi.IsDir():
			// A directory in the untracked listing is a nested git
			// repository: ordinary untracked files are listed one by one,
			// but git will not descend into a repo-within-the-repo, so
			// everything under this entry — working files and the .git
			// with its commits and stashes — is invisible to every git
			// command run on the parent. Hash the bytes wholesale instead
			// of interpreting them: completeness matters here, cleverness
			// does not.
			if err := hashDirectory(ctx, h, full); err != nil {
				return "", fmt.Errorf("fingerprint %s: %w", p, err)
			}
		default:
			// Sockets, fifos, devices: the type in the mode string above
			// is all the identity they have.
		}
	}
	// Submodules, recursively. Their untracked files appear in no diff and
	// no listing of the parent — the parent's status only says the
	// submodule is dirty — so without descending, two different states
	// would fingerprint identically and "identified" would be a lie. An
	// error inside a submodule propagates: a state that cannot be fully
	// identified must not pretend it was.
	subs, err := runRaw(ctx, r.Dir, "submodule", "--quiet", "foreach", `printf '%s\0' "$sm_path"`)
	if err != nil {
		return "", err
	}
	for _, p := range strings.Split(subs, "\x00") {
		if p == "" {
			continue
		}
		subFP, err := (Repo{Dir: filepath.Join(r.Dir, p)}).ChangeFingerprint(ctx)
		if err != nil {
			return "", fmt.Errorf("fingerprint submodule %s: %w", p, err)
		}
		record(p)
		record(subFP)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashFile streams one file into the hash with its size as the length
// prefix, so a multi-gigabyte untracked artifact costs a read buffer, not
// its own size in memory — an OOM kill is the one failure fail-closed can
// never catch, because it never returns as an error. A short or long read
// means the file changed mid-read, which is exactly a state the caller must
// not call identified.
func hashFile(h io.Writer, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(h, "%d:", size)
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s changed while being read", path)
	}
	return nil
}

// hashDirectory records a directory tree byte for byte — path, type, mode,
// and symlink target or file content per entry, in WalkDir's deterministic
// order, every field length-prefixed. Used for untracked nested
// repositories, where no git interpretation of the parent reaches.
func hashDirectory(ctx context.Context, h io.Writer, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		field := func(f string) {
			fmt.Fprintf(h, "%d:", len(f))
			io.WriteString(h, f)
		}
		field(rel)
		field(fi.Mode().String())
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			field(target)
		case fi.Mode().IsRegular():
			if err := hashFile(h, path, fi.Size()); err != nil {
				return err
			}
		}
		return nil
	})
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
// The entry is tagged with a nonce and found by it, not by reading the top
// of the stash afterwards: the user can push their own stash between our
// push and our look, and we would then adopt theirs as ours.
//
// message is set as soon as the push has succeeded, including when the
// lookup afterwards fails: at that point the user's work has already left
// the working tree, and the message is the only handle anyone has on it.
func (r Repo) StashPush(ctx context.Context) (stashID, message string, err error) {
	nonce, err := stashNonce()
	if err != nil {
		return "", "", err
	}
	message = "cepm auto-stash " + nonce
	if _, err := run(ctx, r.Dir, "stash", "push", "--include-untracked", "--message", message); err != nil {
		return "", "", err
	}
	if AfterStashPush != nil {
		AfterStashPush()
	}
	id, _, err := r.findStash(ctx, nonce)
	if err != nil {
		return "", message, err
	}
	return id, message, nil
}

// AfterStashPush runs between creating the auto-stash and identifying it.
// Test-only seam (nil in production): that gap is where a user's own "git
// stash" can land, and nothing else can open it deterministically.
var AfterStashPush func()

// stashNonce is what makes one auto-stash distinguishable from every other
// entry, including a previous cepm run's.
func stashNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate stash nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// findStash locates the entry carrying nonce, returning its commit id and
// current reference. Both are empty when it is not in the list.
func (r Repo) findStash(ctx context.Context, nonce string) (id, ref string, err error) {
	out, err := run(ctx, r.Dir, "stash", "list", "--format=%H %gd %gs")
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(fields) == 3 && strings.Contains(fields[2], nonce) {
			return fields[0], fields[1], nil
		}
	}
	return "", "", nil
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

// StashPop restores the entry StashPush created — never "the top one" — and
// removes nothing. The user (or another tool) can push a stash in the clone
// while cepm is pulling, so applying goes by commit id, which no concurrent
// push can move.
//
// Nothing is dropped because nothing can be dropped safely: git deletes a
// stash entry by position only, and the position can name someone else's
// entry by the time the command runs. It returns the id of the entry it
// left, so the caller can tell the user what to clean up; "" means the
// entry was already gone. On conflict the apply itself fails and the error
// is the caller's to surface.
func (r Repo) StashPop(ctx context.Context, stashID string) (leftBehind string, err error) {
	// By commit id, with no position resolved first: there is then no
	// window in which a concurrent push can redirect what gets applied.
	// The id keeps working even if the entry was dropped meanwhile — the
	// commit outlives the list, and restoring the user's work is the point.
	if _, err := run(ctx, r.Dir, "stash", "apply", stashID); err != nil {
		return "", fmt.Errorf("restoring the auto-stash %s: %w", stashID, err)
	}

	// And then the entry is deliberately left alone. git can only drop a
	// stash by position, and a push landing between resolving that position
	// and using it makes the drop take someone else's entry. Undoing that
	// afterwards is not a fix: between the wrong drop and its restore the
	// user's work exists only in cepm's memory, and a failed "stash store"
	// — or a process that stops right there — loses it for good. Keeping
	// cepm's own entry costs the user a stale line in "git stash list";
	// the alternative can cost them work, so this reports it instead.
	ref, err := r.StashRef(ctx, stashID)
	if err != nil {
		return stashID, nil // cannot tell; assume it is still there
	}
	if ref == "" {
		return "", nil // already gone; nothing to mention
	}
	return stashID, nil
}
