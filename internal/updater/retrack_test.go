package updater

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/state"
)

// setupTagRepo registers the setupRepo fixture in tag mode, pinned at
// v1.0.0. Returns the author clone dir.
func setupTagRepo(t *testing.T, name string) (authorDir string) {
	t.Helper()
	author := setupRepo(t, name)
	tagAt(t, author, "v1.0.0", "2026-01-01T00:00:00")
	git(t, author, "push", "origin", "v1.0.0")

	dir, _ := RepoDir(name)
	st, _ := state.Load()
	r := st.Repos[name]
	r.Track, r.TagPattern, r.Branch = state.TrackTag, "v*", ""
	git(t, dir, "fetch", "--tags", "origin")
	git(t, dir, "checkout", "--detach", "v1.0.0")
	r.Tag, r.Head = "v1.0.0", git(t, dir, "rev-parse", "HEAD")
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	return author
}

func TestRetrackBranchToTag(t *testing.T) {
	author := setupRepo(t, "mytools")
	// A release beyond the registered head, touching alpha.
	writeFile(t, filepath.Join(author, "ext", "alpha", "release.js"), "x")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "release")
	tagAt(t, author, "v1.2.0", "2026-01-02T00:00:00")
	// And a commit after the tag that tag tracking must not follow.
	writeFile(t, filepath.Join(author, "ext", "beta", "wip.js"), "y")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "unreleased")
	git(t, author, "push", "origin", "main", "--tags")

	res, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackTag})
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.Updated || res.NewRef != "v1.2.0" {
		t.Errorf("expected a move to v1.2.0, got %+v", res)
	}
	if len(res.Changed) != 1 || res.Changed[0].Name != "Alpha" {
		t.Errorf("expected only Alpha changed (the unreleased beta commit is past the tag), got %+v", res.Changed)
	}

	dir, _ := RepoDir("mytools")
	st, _ := state.Load()
	r := st.Repos["mytools"]
	if r.Track != state.TrackTag || r.TagPattern != "v*" || r.Tag != "v1.2.0" {
		t.Errorf("state not switched to tag tracking: %+v", r)
	}
	if r.Head != git(t, dir, "rev-parse", "v1.2.0^{commit}") {
		t.Errorf("head %q is not the v1.2.0 commit", r.Head)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != r.Head {
		t.Errorf("clone HEAD %q does not match state head %q", got, r.Head)
	}
	// The recorded branch survives the switch, so switching back needs no
	// --branch.
	if r.Branch != "main" {
		t.Errorf("branch record lost in the switch: %q", r.Branch)
	}
}

func TestRetrackTagToBranch(t *testing.T) {
	author := setupTagRepo(t, "mytools")
	// main moves past the release.
	writeFile(t, filepath.Join(author, "ext", "beta", "new.js"), "z")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "past the tag")
	git(t, author, "push", "origin", "main")

	// No Branch recorded and none passed: the remote default branch decides.
	res, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackBranch})
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.Updated || len(res.Changed) != 1 || res.Changed[0].Name != "Beta" {
		t.Errorf("expected Beta changed by the move to main, got %+v", res)
	}
	if res.OldRef != "v1.0.0" {
		t.Errorf("OldRef %q should name the tag the switch left, not a hash", res.OldRef)
	}

	dir, _ := RepoDir("mytools")
	st, _ := state.Load()
	r := st.Repos["mytools"]
	if r.Track != state.TrackBranch || r.Branch != "main" {
		t.Errorf("state not switched to branch tracking: %+v", r)
	}
	if r.Tag != "" {
		t.Errorf("a checked-out tag is still recorded after leaving tag mode: %q", r.Tag)
	}
	if got := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("clone is on %q, want branch main", got)
	}
	if r.Head != git(t, dir, "rev-parse", "HEAD") {
		t.Errorf("head %q does not match the clone", r.Head)
	}
}

// A clone that has only ever tracked tags — or was cloned on another branch —
// has no local branch for the target; the switch must create it from origin.
func TestRetrackCreatesMissingLocalBranch(t *testing.T) {
	author := setupTagRepo(t, "mytools")
	git(t, author, "checkout", "-b", "develop")
	writeFile(t, filepath.Join(author, "ext", "alpha", "dev.js"), "d")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "dev work")
	git(t, author, "push", "origin", "develop")

	dir, _ := RepoDir("mytools")
	git(t, dir, "branch", "-D", "main") // only the detached tag checkout remains

	res, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackBranch, Branch: "develop"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.Track != state.TrackBranch || r.Branch != "develop" {
		t.Errorf("state not switched to develop: %+v", r)
	}
	if got := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "develop" {
		t.Errorf("clone is on %q, want branch develop", got)
	}
}

// Failing to find a tag to follow must leave both the state and the clone
// exactly as they were: a half-committed switch would strand the repository
// in a mode that can never update.
func TestRetrackWithoutMatchingTagsChangesNothing(t *testing.T) {
	setupRepo(t, "mytools")

	_, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackTag})
	if err == nil || !strings.Contains(err.Error(), "no tags match") {
		t.Fatalf("expected a no-tags refusal, got %v", err)
	}
	dir, _ := RepoDir("mytools")
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.Track != state.TrackBranch || r.Branch != "main" {
		t.Errorf("state changed by a refused switch: %+v", r)
	}
	if got := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("clone moved by a refused switch: on %q", got)
	}
}

// "HEAD" resolves as a revision (origin/HEAD exists in every normal clone)
// but is not a branch git will switch to — it must be refused before the
// state commits to it, not discovered at the switch after.
func TestRetrackRejectsInvalidBranchName(t *testing.T) {
	setupTagRepo(t, "mytools")

	_, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackBranch, Branch: "HEAD"})
	if err == nil || !strings.Contains(err.Error(), "invalid branch name") {
		t.Fatalf("expected an invalid-name refusal, got %v", err)
	}
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.Track != state.TrackTag || r.Branch != "" {
		t.Errorf("state changed by a refused switch: %+v", r)
	}
}

// A local branch with commits of its own can never fast-forward onto the
// remote one; discovering that only after the state committed to branch
// tracking would leave every later update failing.
func TestRetrackRefusesDivergedLocalBranch(t *testing.T) {
	author := setupTagRepo(t, "mytools")
	writeFile(t, filepath.Join(author, "README.md"), "upstream moved")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "upstream")
	git(t, author, "push", "origin", "main")

	// The clone's local main gains its own commit, then returns to the tag.
	dir, _ := RepoDir("mytools")
	git(t, dir, "switch", "main")
	writeFile(t, filepath.Join(dir, "local.txt"), "mine")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "local work")
	git(t, dir, "checkout", "--detach", "v1.0.0")

	_, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackBranch})
	if err == nil || !strings.Contains(err.Error(), "commits that origin/main does not") {
		t.Fatalf("expected a diverged-branch refusal, got %v", err)
	}
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.Track != state.TrackTag {
		t.Errorf("state changed by a refused switch: %+v", r)
	}
}

// A tag can point at a blob; following it would fail at the checkout, after
// the state already committed to tag mode.
func TestRetrackRefusesNonCommitTag(t *testing.T) {
	author := setupRepo(t, "mytools")
	blob := git(t, author, "hash-object", "-w", "README.md")
	git(t, author, "tag", "v9.9.9", blob)
	git(t, author, "push", "origin", "v9.9.9")

	_, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackTag})
	if err == nil || !strings.Contains(err.Error(), "does not name a commit") {
		t.Fatalf("expected a non-commit-tag refusal, got %v", err)
	}
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.Track != state.TrackBranch {
		t.Errorf("state changed by a refused switch: %+v", r)
	}
}

// origin/HEAD is recorded at clone time and fetch leaves it alone, so a
// remote whose default branch moved would silently be misread; the switch
// must ask the remote, not the clone's memory.
func TestRetrackFollowsTheCurrentRemoteDefault(t *testing.T) {
	author := setupTagRepo(t, "mytools")
	git(t, author, "checkout", "-b", "stable")
	git(t, author, "push", "origin", "stable")
	// The bare origin changes its default branch; "main" stays around.
	origin := filepath.Join(filepath.Dir(author), "origin.git")
	git(t, origin, "symbolic-ref", "HEAD", "refs/heads/stable")

	res, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackBranch})
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.Branch != "stable" {
		t.Errorf("followed %q, want the remote's current default branch \"stable\"", r.Branch)
	}
}

// The recovery path the errors promise: a crash (or failure) between the
// intent save and the switch leaves branch tracking saved with a detached
// clone, and re-running track must finish the move.
func TestRetrackFinishesAnInterruptedBranchSwitch(t *testing.T) {
	setupTagRepo(t, "mytools")
	st, _ := state.Load()
	r := st.Repos["mytools"]
	r.Track, r.Branch, r.Tag = state.TrackBranch, "main", ""
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	res, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackBranch})
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	dir, _ := RepoDir("mytools")
	if got := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("clone still on %q; the re-run must finish the interrupted switch", got)
	}
}

// The same guarantee when the target branch does not exist upstream.
func TestRetrackWithUnknownBranchChangesNothing(t *testing.T) {
	setupTagRepo(t, "mytools")

	_, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackBranch, Branch: "nope"})
	if err == nil || !strings.Contains(err.Error(), "not found on origin") {
		t.Fatalf("expected an unknown-branch refusal, got %v", err)
	}
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.Track != state.TrackTag || r.Tag != "v1.0.0" {
		t.Errorf("state changed by a refused switch: %+v", r)
	}
}

// A tag named "origin/<branch>" answers for the short ref "origin/<branch>",
// so existence checked through it would pass for a remote branch that is not
// there — and the switch would fail after the intent save.
func TestRetrackIsNotFooledByATagNamedLikeARemoteBranch(t *testing.T) {
	author := setupTagRepo(t, "mytools")
	git(t, author, "tag", "origin/ghost")
	git(t, author, "push", "origin", "origin/ghost")

	_, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackBranch, Branch: "ghost"})
	if err == nil || !strings.Contains(err.Error(), "not found on origin") {
		t.Fatalf("expected an unknown-branch refusal, got %v", err)
	}
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.Track != state.TrackTag {
		t.Errorf("state changed by a refused switch: %+v", r)
	}
}

// set-head failing locally (a leftover lock file) must not degrade into
// silently following the clone-time default branch; the user is sent to
// --branch instead, with nothing changed.
func TestRetrackRefusesAStaleDefaultBranchAnswer(t *testing.T) {
	setupTagRepo(t, "mytools")
	dir, _ := RepoDir("mytools")
	writeFile(t, filepath.Join(dir, ".git", "refs", "remotes", "origin", "HEAD.lock"), "")

	_, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackBranch})
	if err == nil || !strings.Contains(err.Error(), "pass --branch") {
		t.Fatalf("expected a pass---branch refusal, got %v", err)
	}
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.Track != state.TrackTag {
		t.Errorf("state changed by a refused switch: %+v", r)
	}
}

func TestRetrackRefusesDirtyTree(t *testing.T) {
	setupRepo(t, "mytools")
	dir, _ := RepoDir("mytools")
	writeFile(t, filepath.Join(dir, "ext", "alpha", "local-edit.js"), "mine")

	_, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackTag})
	if err == nil || !strings.Contains(err.Error(), "local changes") {
		t.Fatalf("expected a dirty-tree refusal, got %v", err)
	}
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.Track != state.TrackBranch {
		t.Errorf("state changed despite the dirty tree: %+v", r)
	}
}

func TestRetrackUnknownRepo(t *testing.T) {
	setupRepo(t, "mytools")
	_, err := Retrack(context.Background(), "nosuch", RetrackOptions{Track: state.TrackTag})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected an unknown-repo error, got %v", err)
	}
}

// Tag-mode settings can be changed without switching modes: the pattern is
// re-resolved and the clone moves to the newly matching release.
func TestRetrackChangesTagPattern(t *testing.T) {
	author := setupTagRepo(t, "mytools")
	writeFile(t, filepath.Join(author, "ext", "alpha", "v2.js"), "2")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "2.0")
	tagAt(t, author, "v2.0.0", "2026-01-05T00:00:00")
	git(t, author, "push", "origin", "main", "--tags")

	// Pin to the 1.x line first: the newer v2.0.0 must not be followed.
	res, err := Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackTag, TagPattern: "v1.*"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Updated {
		t.Errorf("v1.* must stay on v1.0.0, got %+v", res)
	}
	st, _ := state.Load()
	if r := st.Repos["mytools"]; r.TagPattern != "v1.*" || r.Tag != "v1.0.0" {
		t.Errorf("pattern change not recorded: %+v", r)
	}

	// Widening the pattern picks the 2.x release up.
	res, err = Retrack(context.Background(), "mytools", RetrackOptions{Track: state.TrackTag, TagPattern: "v*"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.Updated || res.NewRef != "v2.0.0" {
		t.Errorf("expected a move to v2.0.0, got %+v", res)
	}
}
