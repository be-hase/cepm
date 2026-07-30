// Package gitx wraps the git CLI. It deliberately shells out to git instead of
// using go-git so that the user's SSH config, credential helpers, and proxy
// settings work unchanged with internal git servers.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Repo is a git working tree at Dir.
type Repo struct {
	Dir string
}

func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmdArgs := args
	if dir != "" {
		cmdArgs = append([]string{"-C", dir}, args...)
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
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
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
func (r Repo) Fetch(ctx context.Context) error {
	_, err := run(ctx, r.Dir, "fetch", "--tags", "--prune", "origin")
	return err
}

// MergeFFOnly fast-forwards the current branch to ref, failing if a
// fast-forward is not possible (e.g. rewritten history).
func (r Repo) MergeFFOnly(ctx context.Context, ref string) error {
	_, err := run(ctx, r.Dir, "merge", "--ff-only", ref)
	return err
}

// TagsByCreation returns tags matching the glob pattern, most recently
// created first.
func (r Repo) TagsByCreation(ctx context.Context, pattern string) ([]string, error) {
	out, err := run(ctx, r.Dir, "tag", "--list", pattern, "--sort=-creatordate")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// CheckoutDetached checks out ref as a detached HEAD.
func (r Repo) CheckoutDetached(ctx context.Context, ref string) error {
	_, err := run(ctx, r.Dir, "checkout", "--detach", "--quiet", ref)
	return err
}

// Checkout checks out a branch.
func (r Repo) Checkout(ctx context.Context, branch string) error {
	_, err := run(ctx, r.Dir, "checkout", "--quiet", branch)
	return err
}

// ChangedFiles lists paths (repo-relative) that differ between two commits.
func (r Repo) ChangedFiles(ctx context.Context, from, to string) ([]string, error) {
	out, err := run(ctx, r.Dir, "diff", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// StashPush stashes local changes including untracked files.
func (r Repo) StashPush(ctx context.Context) error {
	_, err := run(ctx, r.Dir, "stash", "push", "--include-untracked", "--message", "cepm auto-stash")
	return err
}

// StashPop restores the most recent stash. On conflict the stash entry is
// kept by git; callers should surface the error to the user.
func (r Repo) StashPop(ctx context.Context) error {
	_, err := run(ctx, r.Dir, "stash", "pop")
	return err
}
