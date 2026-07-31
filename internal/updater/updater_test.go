package updater

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/extid"
	"github.com/be-hase/cepm/internal/gitx"
	"github.com/be-hase/cepm/internal/state"
)

// git runs a git command in dir with identity/env pinned for reproducibility.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeManifest(t *testing.T, dir, name string) {
	writeFile(t, filepath.Join(dir, "manifest.json"),
		`{"manifest_version": 3, "name": "`+name+`", "version": "1.0"}`)
}

// setupRepo creates a bare "origin", an author clone to push from, and a
// cepm-managed clone registered in state.json. Returns the author clone dir.
func setupRepo(t *testing.T, name string) (authorDir string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("CEPM_HOME", filepath.Join(base, "cepmhome"))

	bare := filepath.Join(base, "origin.git")
	git(t, base, "init", "--bare", "--initial-branch=main", bare)

	authorDir = filepath.Join(base, "author")
	git(t, base, "clone", bare, authorDir)
	git(t, authorDir, "checkout", "-b", "main")
	writeManifest(t, filepath.Join(authorDir, "ext", "alpha"), "Alpha")
	writeManifest(t, filepath.Join(authorDir, "ext", "beta"), "Beta")
	writeFile(t, filepath.Join(authorDir, "README.md"), "hi")
	git(t, authorDir, "add", "-A")
	git(t, authorDir, "commit", "-m", "initial")
	git(t, authorDir, "push", "origin", "main")

	cloneDir, err := RepoDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cloneDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := gitx.Clone(context.Background(), bare, cloneDir, "main"); err != nil {
		t.Fatal(err)
	}
	head := git(t, cloneDir, "rev-parse", "HEAD")

	// Use the ids cepm would really compute: a fixture with made-up ids would
	// look like every extension had just changed identity.
	alphaID, err := extid.FromPath(filepath.Join(cloneDir, "ext", "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	betaID, err := extid.FromPath(filepath.Join(cloneDir, "ext", "beta"))
	if err != nil {
		t.Fatal(err)
	}
	st := state.New()
	st.Repos[name] = &state.Repo{
		URL:    bare,
		Track:  state.TrackBranch,
		Branch: "main",
		Head:   head,
		Extensions: []state.Extension{
			{Dir: filepath.Join("ext", "alpha"), Name: "Alpha", ID: alphaID},
			{Dir: filepath.Join("ext", "beta"), Name: "Beta", ID: betaID},
		},
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	return authorDir
}

func TestUpdateNoChanges(t *testing.T) {
	setupRepo(t, "mytools")
	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Err != nil || results[0].Updated {
		t.Errorf("unexpected results: %+v", results)
	}
}

func TestUpdateAttributesChangedExtensions(t *testing.T) {
	author := setupRepo(t, "mytools")
	writeFile(t, filepath.Join(author, "ext", "alpha", "content.js"), "console.log(1)")
	writeFile(t, filepath.Join(author, "README.md"), "changed but not an extension")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "update alpha")
	git(t, author, "push", "origin", "main")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !r.Updated || len(r.Changed) != 1 || r.Changed[0].Name != "Alpha" {
		t.Errorf("expected only Alpha changed, got %+v", r)
	}
	if r.Changed[0].ID == "" || len(r.Changed[0].ID) != 32 {
		t.Errorf("changed extension should carry a computed ID, got %q", r.Changed[0].ID)
	}

	// State must be persisted with the new head and refreshed IDs.
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Repos["mytools"].LastError != "" || st.Repos["mytools"].Head == "" {
		t.Errorf("state not updated: %+v", st.Repos["mytools"])
	}
}

func TestUpdateNewExtensionIsAvailable(t *testing.T) {
	author := setupRepo(t, "mytools")
	writeManifest(t, filepath.Join(author, "ext", "gamma"), "Gamma")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "add gamma")
	git(t, author, "push", "origin", "main")

	if _, err := Update(context.Background(), nil, Options{}); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load()
	gamma := st.Repos["mytools"].FindExtension(filepath.Join("ext", "gamma"))
	if gamma == nil || gamma.Enabled() {
		t.Errorf("new extension should be registered as available (disabled), got %+v", gamma)
	}
	// Pre-existing extensions keep their enabled intent.
	alpha := st.Repos["mytools"].FindExtension(filepath.Join("ext", "alpha"))
	if alpha == nil || !alpha.Enabled() {
		t.Errorf("existing extension should stay enabled, got %+v", alpha)
	}
}

func TestUpdateSkipsDisabledExtensionChanges(t *testing.T) {
	author := setupRepo(t, "mytools")
	// Mark beta as available (user opted out).
	st, _ := state.Load()
	st.Repos["mytools"].FindExtension(filepath.Join("ext", "beta")).Disabled = true
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(author, "ext", "beta", "x.js"), "x")
	writeFile(t, filepath.Join(author, "ext", "alpha", "y.js"), "y")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "update both")
	git(t, author, "push", "origin", "main")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if len(r.Changed) != 1 || r.Changed[0].Name != "Alpha" {
		t.Errorf("disabled extension must not be a reload target, got %+v", r.Changed)
	}
	// The re-scan must not resurrect the disabled flag.
	st2, _ := state.Load()
	if st2.Repos["mytools"].FindExtension(filepath.Join("ext", "beta")).Enabled() {
		t.Error("disabled intent was lost across an update")
	}
}

// A malformed "key" in a pulled commit must not cost the user their
// registration: dropping just that extension would lose its enable choice and
// mark it for removal from Chrome.
func TestUpdateKeepsExtensionsWhenAKeyIsInvalid(t *testing.T) {
	author := setupRepo(t, "mytools")
	writeFile(t, filepath.Join(author, "ext", "alpha", "manifest.json"),
		`{"manifest_version":3,"name":"Alpha","version":"1.0","key":"not-valid-base64!!"}`)
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "bad key")
	git(t, author, "push", "origin", "main")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err == nil {
		t.Error("an invalid key should be reported as an error")
	}
	st, _ := state.Load()
	repo := st.Repos["mytools"]
	if repo.FindExtension(filepath.Join("ext", "alpha")) == nil {
		t.Error("the extension must stay registered")
	}
	if len(repo.Stale) != 0 {
		t.Errorf("nothing should be marked for cleanup: %+v", repo.Stale)
	}
}

// Adding a key changes the id Chrome uses, so the old entry is dead and the
// new one needs loading once.
func TestUpdateReportsIdentityChangeWhenKeyAppears(t *testing.T) {
	author := setupRepo(t, "mytools")
	st, _ := state.Load()
	oldID := st.Repos["mytools"].FindExtension(filepath.Join("ext", "alpha")).ID

	const key = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA0lLejiTvG5ElQmwA+FNOPTFTArbjNA65OVcj5zk3efV/myX/PK/TWO7oGT1BE/9zZfbozbaAMwrk6l8FoRVMGqmPaPCfdDdbtJ+ogS+6Evw9EJ3Tx+2oLUS+ddyzLbsMkoeXe0wvDIX4vOnwi1tULgTpxBlsSQ2zF5e8oZG+wMZRb3s8iPDwskfxrqFSgAaDuNH1vmZiRzOqnz+uLNwdjGHpMrP4KTeGbrAW71EBhYFT0eT47ScdgYodPS1LnfnIobpC5ALPIsIcJnDPKNfL//rlfi4/pGXRq08jOSb1z9nz4sMNTfiHl7shswdTSM1aUu9rsIF1fWmJPXVdQ2IbZQIDAQAB"
	writeFile(t, filepath.Join(author, "ext", "alpha", "manifest.json"),
		`{"manifest_version":3,"name":"Alpha","version":"1.0","key":"`+key+`"}`)
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "pin id")
	git(t, author, "push", "origin", "main")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results[0].Reidentified) != 1 {
		t.Fatalf("expected an identity change, got %+v", results[0].Reidentified)
	}
	if got := results[0].Reidentified[0].OldID; got != oldID {
		t.Errorf("old id = %q, want %q", got, oldID)
	}
	st2, _ := state.Load()
	repo := st2.Repos["mytools"]
	if repo.FindExtension(filepath.Join("ext", "alpha")).ID == oldID {
		t.Error("the stored id should be the key-derived one now")
	}
	// The dead Chrome entry has to be cleanable.
	found := false
	for _, s := range repo.Stale {
		if s.ID == oldID {
			found = true
		}
	}
	if !found {
		t.Errorf("the old id should be recorded for cleanup: %+v", repo.Stale)
	}
}

// A key pushed to one repo that collides with an id another repo already
// registers must fail that repo's refresh without corrupting either.
func TestUpdateRefusesKeyCollisionAcrossRepos(t *testing.T) {
	const key = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA0lLejiTvG5ElQmwA+FNOPTFTArbjNA65OVcj5zk3efV/myX/PK/TWO7oGT1BE/9zZfbozbaAMwrk6l8FoRVMGqmPaPCfdDdbtJ+ogS+6Evw9EJ3Tx+2oLUS+ddyzLbsMkoeXe0wvDIX4vOnwi1tULgTpxBlsSQ2zF5e8oZG+wMZRb3s8iPDwskfxrqFSgAaDuNH1vmZiRzOqnz+uLNwdjGHpMrP4KTeGbrAW71EBhYFT0eT47ScdgYodPS1LnfnIobpC5ALPIsIcJnDPKNfL//rlfi4/pGXRq08jOSb1z9nz4sMNTfiHl7shswdTSM1aUu9rsIF1fWmJPXVdQ2IbZQIDAQAB"
	author := setupRepo(t, "mytools")

	// Another repo already owns the id this key derives to.
	keyID, err := extid.ForExtension("/irrelevant-with-key", key)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load()
	st.Repos["other"] = &state.Repo{
		URL: "u2", Track: state.TrackBranch, Branch: "main", Head: "abc",
		Extensions: []state.Extension{{Dir: "pinned", Name: "Pinned", ID: keyID, Key: key}},
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(author, "ext", "alpha", "manifest.json"),
		`{"manifest_version":3,"name":"Alpha","version":"1.0","key":"`+key+`"}`)
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "collide")
	git(t, author, "push", "origin", "main")

	results, err := Update(context.Background(), []string{"mytools"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err == nil {
		t.Fatal("the colliding refresh must fail")
	}
	st2, _ := state.Load()
	if st2.Repos["mytools"].FindExtension(filepath.Join("ext", "alpha")) == nil {
		t.Error("mytools' registration must survive the failed refresh")
	}
	if st2.Repos["other"].FindExtension("pinned") == nil {
		t.Error("the other repo must be untouched")
	}
}

// A release can be re-tagged onto a new commit; following the tag name alone
// would leave the working tree on the old one forever.
func TestUpdateFollowsForceMovedTag(t *testing.T) {
	author := setupRepo(t, "mytools")
	tagAt(t, author, "v1.0.0", "2026-01-01T00:00:00")
	git(t, author, "push", "origin", "v1.0.0")

	dir, _ := RepoDir("mytools")
	st, _ := state.Load()
	r := st.Repos["mytools"]
	r.Track, r.TagPattern, r.Branch = state.TrackTag, "v*", ""
	git(t, dir, "fetch", "--tags", "origin")
	git(t, dir, "checkout", "--detach", "v1.0.0")
	r.Tag, r.Head = "v1.0.0", git(t, dir, "rev-parse", "HEAD")
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(author, "ext", "alpha", "fix.js"), "x")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "hotfix")
	git(t, author, "tag", "-f", "-a", "v1.0.0", "-m", "v1.0.0")
	git(t, author, "push", "--force", "origin", "main", "--tags")
	want := git(t, author, "rev-parse", "v1.0.0^{commit}")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err != nil {
		t.Fatal(results[0].Err)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != want {
		t.Errorf("working tree is at %s, want the tag's new commit %s", got, want)
	}
}

// The clone belongs to cepm, but users do poke at it; merging origin/main
// into whatever they checked out would rewrite their branch.
func TestUpdateRefusesWhenOnAnotherBranch(t *testing.T) {
	setupRepo(t, "mytools")
	dir, _ := RepoDir("mytools")
	git(t, dir, "checkout", "-q", "-b", "my-experiment")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err == nil {
		t.Fatal("update should refuse to run on a different branch")
	}
	if branch := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); branch != "my-experiment" {
		t.Errorf("the user's branch was changed to %q", branch)
	}
}

func TestUpdateDetectsRename(t *testing.T) {
	author := setupRepo(t, "mytools")
	git(t, author, "mv", filepath.Join("ext", "alpha"), filepath.Join("ext", "alpha-v2"))
	git(t, author, "commit", "-m", "rename alpha dir")
	git(t, author, "push", "origin", "main")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if len(r.Renamed) != 1 {
		t.Fatalf("expected 1 rename, got %+v (added=%v removed=%v)", r.Renamed, r.Added, r.Removed)
	}
	rn := r.Renamed[0]
	if rn.Name != "Alpha" || rn.OldDir != filepath.Join("ext", "alpha") || rn.NewDir != filepath.Join("ext", "alpha-v2") {
		t.Errorf("unexpected rename: %+v", rn)
	}
	if !rn.Enabled {
		t.Error("rename must inherit the enabled intent")
	}
	if len(r.Added) != 0 || len(r.Removed) != 0 {
		t.Errorf("rename should not be double-reported: added=%v removed=%v", r.Added, r.Removed)
	}

	st, _ := state.Load()
	repo := st.Repos["mytools"]
	newExt := repo.FindExtension(filepath.Join("ext", "alpha-v2"))
	if newExt == nil || !newExt.Enabled() {
		t.Errorf("renamed extension should exist and stay enabled: %+v", newExt)
	}
	if len(repo.Stale) != 1 || repo.Stale[0].Reason != "renamed" || repo.Stale[0].ID != rn.OldID {
		t.Errorf("old Chrome entry should be recorded as stale: %+v", repo.Stale)
	}
}

func TestUpdateRecordsRemovedAsStale(t *testing.T) {
	author := setupRepo(t, "mytools")
	git(t, author, "rm", "-r", filepath.Join("ext", "beta"))
	git(t, author, "commit", "-m", "drop beta")
	git(t, author, "push", "origin", "main")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results[0].Removed) != 1 || results[0].Removed[0].Name != "Beta" {
		t.Errorf("expected Beta removed, got %+v", results[0].Removed)
	}
	st, _ := state.Load()
	repo := st.Repos["mytools"]
	if len(repo.Stale) != 1 || repo.Stale[0].Reason != "removed" || repo.Stale[0].Name != "Beta" {
		t.Errorf("removed extension should be stale: %+v", repo.Stale)
	}
	if repo.FindExtension(filepath.Join("ext", "beta")) != nil {
		t.Error("removed extension should not stay registered")
	}
}

func TestUpdateDetectsNewExtension(t *testing.T) {
	author := setupRepo(t, "mytools")
	writeManifest(t, filepath.Join(author, "ext", "gamma"), "Gamma")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "add gamma")
	git(t, author, "push", "origin", "main")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if len(r.Added) != 1 || r.Added[0].Name != "Gamma" {
		t.Errorf("expected Gamma reported as added, got %+v", r)
	}
	// A brand-new extension needs a manual load; it must not be in Changed.
	for _, c := range r.Changed {
		if c.Name == "Gamma" {
			t.Error("newly added extension should not be reported as changed")
		}
	}
}

func TestUpdateSkipsDirtyTree(t *testing.T) {
	setupRepo(t, "mytools")
	dir, _ := RepoDir("mytools")
	writeFile(t, filepath.Join(dir, "ext", "alpha", "local-hack.js"), "local change")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Skipped {
		t.Errorf("expected dirty repo to be skipped, got %+v", results[0])
	}
}

func TestUpdateStashesDirtyTreeWhenForced(t *testing.T) {
	author := setupRepo(t, "mytools")
	writeFile(t, filepath.Join(author, "ext", "beta", "new.js"), "x")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "update beta")
	git(t, author, "push", "origin", "main")

	dir, _ := RepoDir("mytools")
	writeFile(t, filepath.Join(dir, "local-note.txt"), "keep me")

	results, err := Update(context.Background(), nil, Options{StashDirty: true})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.Err != nil || !r.Updated {
		t.Fatalf("expected forced update to succeed, got %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "local-note.txt")); err != nil {
		t.Error("local change should be restored after stash pop")
	}
}

func TestUpdateFailsOnRewrittenHistory(t *testing.T) {
	author := setupRepo(t, "mytools")
	git(t, author, "commit", "--amend", "-m", "rewritten")
	git(t, author, "push", "--force", "origin", "main")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err == nil {
		t.Error("expected ff-only failure on rewritten history")
	}
	st, _ := state.Load()
	if st.Repos["mytools"].LastError == "" {
		t.Error("lastError should be recorded in state")
	}
}

func TestUpdateUnknownRepo(t *testing.T) {
	setupRepo(t, "mytools")
	results, err := Update(context.Background(), []string{"nope"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err == nil {
		t.Error("expected error for unknown repo name")
	}
}

func tagAt(t *testing.T, dir, tag, date string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "tag", "-a", tag, "-m", tag)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_COMMITTER_DATE="+date,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git tag %s: %v\n%s", tag, err, out)
	}
}

func TestUpdateTagMode(t *testing.T) {
	author := setupRepo(t, "mytools")
	tagAt(t, author, "v1.0.0", "2026-01-01T00:00:00")
	git(t, author, "push", "origin", "v1.0.0")

	// Switch the registered repo to tag mode, pinned at v1.0.0.
	dir, _ := RepoDir("mytools")
	st, _ := state.Load()
	r := st.Repos["mytools"]
	r.Track = state.TrackTag
	r.TagPattern = "v*"
	r.Branch = ""
	git(t, dir, "fetch", "--tags", "origin")
	git(t, dir, "checkout", "--detach", "v1.0.0")
	r.Tag = "v1.0.0"
	r.Head = git(t, dir, "rev-parse", "HEAD")
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	// New commits: v1.9.0, then v1.10.0 (created later; semver must pick
	// v1.10.0 which lexicographic sorting would get wrong).
	writeFile(t, filepath.Join(author, "ext", "alpha", "v19.js"), "x")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "1.9")
	tagAt(t, author, "v1.9.0", "2026-01-02T00:00:00")
	writeFile(t, filepath.Join(author, "ext", "beta", "v110.js"), "y")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "1.10")
	tagAt(t, author, "v1.10.0", "2026-01-03T00:00:00")
	// Also an unstable tag that must not match the pattern.
	tagAt(t, author, "nightly-2026", "2026-01-04T00:00:00")
	git(t, author, "push", "origin", "main", "--tags")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res := results[0]
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.Updated || res.NewRef != "v1.10.0" {
		t.Errorf("expected update to v1.10.0, got %+v", res)
	}
	// Both alpha (1.9) and beta (1.10) changed between v1.0.0 and v1.10.0.
	if len(res.Changed) != 2 {
		t.Errorf("expected 2 changed extensions, got %+v", res.Changed)
	}
	st2, _ := state.Load()
	if st2.Repos["mytools"].Tag != "v1.10.0" {
		t.Errorf("state tag = %q, want v1.10.0", st2.Repos["mytools"].Tag)
	}
}

// Publishing a GitHub release pushes its tag, so following stable tags is
// equivalent to following published stable releases — verified here against a
// real repository.
func TestUpdateTagModeIgnoresPrereleaseTags(t *testing.T) {
	author := setupRepo(t, "mytools")
	tagAt(t, author, "v1.0.0", "2026-01-01T00:00:00")
	git(t, author, "push", "origin", "v1.0.0")

	dir, _ := RepoDir("mytools")
	st, _ := state.Load()
	r := st.Repos["mytools"]
	r.Track, r.TagPattern, r.Branch = state.TrackTag, "v*", ""
	git(t, dir, "fetch", "--tags", "origin")
	git(t, dir, "checkout", "--detach", "v1.0.0")
	r.Tag, r.Head = "v1.0.0", git(t, dir, "rev-parse", "HEAD")
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	// A release candidate is published after a stable version.
	writeFile(t, filepath.Join(author, "ext", "alpha", "stable.js"), "x")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "1.1.0")
	tagAt(t, author, "v1.1.0", "2026-01-02T00:00:00")
	writeFile(t, filepath.Join(author, "ext", "alpha", "wip.js"), "y")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "2.0.0 rc")
	tagAt(t, author, "v2.0.0-rc1", "2026-01-03T00:00:00")
	git(t, author, "push", "origin", "main", "--tags")

	results, err := Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err != nil {
		t.Fatal(results[0].Err)
	}
	if results[0].NewRef != "v1.1.0" {
		t.Errorf("prerelease tag must not be followed: got %q, want v1.1.0", results[0].NewRef)
	}

	// Opting in picks the release candidate up.
	st2, _ := state.Load()
	st2.Repos["mytools"].Prerelease = true
	if err := st2.Save(); err != nil {
		t.Fatal(err)
	}
	results, err = Update(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].NewRef != "v2.0.0-rc1" {
		t.Errorf("with --prerelease: got %q, want v2.0.0-rc1", results[0].NewRef)
	}
}

func TestLatestTag(t *testing.T) {
	// Input is creatordate-descending, as produced by gitx.TagsByCreation.
	if got, warn := LatestTag([]string{"v1.9.0", "v1.10.0", "v1.0.0"}, false); got != "v1.10.0" || warn != "" {
		t.Errorf("semver: got (%q, %q)", got, warn)
	}
	if got, _ := LatestTag([]string{"2.0.0", "1.9.0"}, false); got != "2.0.0" {
		t.Errorf("semver without v prefix: got %q", got)
	}
	// Oddly named tags are ignored in favour of real version numbers, so a
	// stray "nightly" tag can never become the followed release.
	if got, warn := LatestTag([]string{"release-b", "v1.0.0"}, false); got != "v1.0.0" || warn == "" {
		t.Errorf("mixed tags should prefer the version number with a warning, got (%q, %q)", got, warn)
	}
	// With no version-like tag at all, refuse rather than ship whatever was
	// tagged last: following that would not be "tracking releases".
	if got, warn := LatestTag([]string{"beta", "alpha"}, false); got != "" || warn == "" {
		t.Errorf("non-semver-only tags should be refused with an explanation, got (%q, %q)", got, warn)
	}
}

// A published GitHub release always creates its tag, a draft never does, and
// a prerelease is spelled out by semver — so tag names alone are enough to
// follow "published stable releases" without calling the API.
func TestLatestTagPrereleases(t *testing.T) {
	tags := []string{"v2.0.0-rc2", "v2.0.0-rc1", "v1.10.0", "v1.9.0"}

	if got, warn := LatestTag(tags, false); got != "v1.10.0" || warn != "" {
		t.Errorf("stable-only: got (%q, %q), want v1.10.0", got, warn)
	}
	if got, _ := LatestTag(tags, true); got != "v2.0.0-rc2" {
		t.Errorf("with prereleases: got %q, want v2.0.0-rc2", got)
	}
	// Build metadata is not a prerelease marker.
	if got, _ := LatestTag([]string{"v1.2.0+build7"}, false); got != "v1.2.0+build7" {
		t.Errorf("build metadata should not be treated as prerelease, got %q", got)
	}
	// Nothing but prereleases: report why rather than silently following one.
	got, warn := LatestTag([]string{"v2.0.0-rc1"}, false)
	if got != "" || warn == "" {
		t.Errorf("prerelease-only: got (%q, %q), want (\"\", explanation)", got, warn)
	}
	// Mixed naming must also respect the prerelease filter.
	if got, _ := LatestTag([]string{"v2.0.0-rc1", "nightly", "v1.0.0"}, false); got != "v1.0.0" {
		t.Errorf("mixed naming with prereleases: got %q, want v1.0.0", got)
	}
}

func TestLockSerializes(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithLock(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := WithLock(ctx, func() error { return nil })
	if err == nil {
		t.Error("second WithLock should not enter while the first holds the lock")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
