package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/testgit"
)

// Repository URLs reach the terminal, the log file and doctor output, which
// people paste into chats — a token in one of them must never survive.
func TestRedactURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://user:TOKEN@ghe.example.com/t/r.git", "https://***@ghe.example.com/t/r.git"},
		{"https://ghp_abcdef123@github.com/t/r.git", "https://***@github.com/t/r.git"},
		{"https://x-access-token:ghs_zzz@github.com/t/r", "https://***@github.com/t/r"},
		{"https://github.com/t/r.git", "https://github.com/t/r.git"},
		{"git@github.com:t/r.git", "git@github.com:t/r.git"}, // no credentials
		{
			"clone https://u:p@h/x.git failed: fatal: could not read from https://u:p@h/x.git",
			"clone https://***@h/x.git failed: fatal: could not read from https://***@h/x.git",
		},
		// Tokens travel in queries and fragments too, under names nobody can
		// enumerate — both are replaced wholesale, with no userinfo present.
		{"https://git.example.com/repo.git?access_token=SECRET", "https://git.example.com/repo.git?***"},
		{"https://git.example.com/repo.git#token=SECRET", "https://git.example.com/repo.git#***"},
		{
			"https://user:TOKEN@git.example.com/repo.git?private_token=ANOTHER#t=THIRD",
			"https://***@git.example.com/repo.git?***#***",
		},
		{"https://h/x?redirect=https%3A%2F%2Fevil%2F%3Ftoken%3DSECRET", "https://h/x?***"},
		{
			"fetch https://h/a?x=SECRET and https://u:p@h/b failed",
			"fetch https://h/a?*** and https://***@h/b failed",
		},
		// Unparsable (a stray %-escape): cannot be split into safe and
		// secret parts, so the whole URL is replaced — fail closed.
		{"https://user:TOKEN@example.com/%ZZ?access_token=SECRET", "***"},
		{
			"clone https://user:TOKEN@h/%ZZ failed; see https://h/docs",
			"clone *** failed; see https://h/docs",
		},
	}
	for _, tt := range tests {
		if got := RedactURL(tt.in); got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Git passes SSH's stderr through verbatim, and errors from this package
// reach the terminal — so a hostile server's escape-laden banner must come
// back defanged. A fake SSH stands in for that server; git's own path
// quoting never fires here, so the raw bytes really do reach run().
func TestErrorsCarryNoRawControlCharacters(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-ssh")
	// The banner also forges cepm-looking lines after newlines: a success
	// row, a top-level error, a pasteable command.
	banner := "#!/bin/sh\n" +
		"printf 'remote: \\033]0;pwned\\007 fatal\\n" +
		"\\342\\234\\224 reloaded evil\\n" +
		"Error: forged\\n" +
		"$ rm -rf /\\n' >&2\nexit 128\n"
	if err := os.WriteFile(script, []byte(banner), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", script)

	err := Clone(context.Background(), "ssh://example.invalid/repo.git", filepath.Join(dir, "dst"), "")
	if err == nil {
		t.Fatal("clone through the failing fake SSH should fail")
	}
	msg := err.Error()
	if strings.ContainsRune(msg, 0x1b) || strings.ContainsRune(msg, 0x07) {
		t.Errorf("error carries a raw escape sequence: %q", msg)
	}
	if !strings.Contains(msg, "pwned") {
		t.Errorf("fixture missed: the banner text should appear in %q", msg)
	}
	if !strings.Contains(msg, `\x1b`) {
		t.Errorf("the escaped form should appear in %q", msg)
	}
	// Remote newlines must not mint lines that pose as cepm's own output:
	// every forged line has to arrive visibly quoted.
	for _, forged := range []string{"✔ reloaded evil", "Error: forged", "$ rm -rf /"} {
		if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(forged)).MatchString(msg) {
			t.Errorf("a remote-forged line poses as cepm output:\n%s", msg)
		}
		if !strings.Contains(msg, forged) {
			t.Errorf("fixture missed: %q should appear (quoted) in %q", forged, msg)
		}
	}
}

// gitIn runs a git command in dir with a pinned identity.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = testgit.Env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The auto-stash must be restored by identity, not by position. A user (or
// another tool) can stash in the same clone while cepm is pulling; popping
// "the top one" would then apply their work, delete their entry, and leave
// cepm's own changes stranded in the stash.
func TestStashPopRestoresOurEntryNotWhateverIsOnTop(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "ours"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "theirs"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", "base")

	repo := Repo{Dir: dir}
	ctx := context.Background()

	// cepm's auto-stash of the user's in-progress edit.
	if err := os.WriteFile(filepath.Join(dir, "ours"), []byte("cepm stashed this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stashID, _, err := repo.StashPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stashID == "" {
		t.Fatal("a dirty tree should have produced a stash")
	}

	// Meanwhile the user stashes something of their own; it lands on top.
	if err := os.WriteFile(filepath.Join(dir, "theirs"), []byte("user stashed this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "stash", "push", "-m", "the user's own stash")

	if _, err := repo.StashPop(ctx, stashID); err != nil {
		t.Fatalf("popping our own stash: %v", err)
	}

	ours, err := os.ReadFile(filepath.Join(dir, "ours"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ours) != "cepm stashed this\n" {
		t.Errorf("cepm's own change was not restored, file is %q", ours)
	}
	theirs, err := os.ReadFile(filepath.Join(dir, "theirs"))
	if err != nil {
		t.Fatal(err)
	}
	if string(theirs) != "committed\n" {
		t.Errorf("the user's stash was applied to the working tree: %q", theirs)
	}
	if list := gitIn(t, dir, "stash", "list"); !strings.Contains(list, "the user's own stash") {
		t.Errorf("the user's stash must still be there, list is:\n%s", list)
	}
	// cepm's own entry stays by design (see StashPop); what matters here
	// is that the user's did too, and that theirs was not applied.
}

// A stash entry dropped by hand between push and pop can still be restored:
// the commit outlives the list, and giving the user their work back is the
// point — silently taking whatever else is on the stack is not.
func TestStashPopStillRestoresADroppedEntry(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", "base")

	repo := Repo{Dir: dir}
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stashID, _, err := repo.StashPush(ctx)
	if err != nil || stashID == "" {
		t.Fatalf("StashPush: %q %v", stashID, err)
	}
	gitIn(t, dir, "stash", "drop")

	if _, err := repo.StashPop(ctx, stashID); err != nil {
		t.Fatalf("a dropped entry should still be restorable by id: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "edited\n" {
		t.Errorf("the change was not restored, file is %q", b)
	}
}

// The reviewer's reproduction: a stash pushed after cepm resolved its own
// entry's position, but before the pop. Applying by commit id makes the
// position irrelevant, so the interloper is neither applied nor dropped.
func TestStashPopIgnoresAStashPushedAfterResolution(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--initial-branch=main")
	for _, f := range []string{"ours", "one", "two"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("committed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", "base")

	repo := Repo{Dir: dir}
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "ours"), []byte("cepm stashed this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stashID, _, err := repo.StashPush(ctx)
	if err != nil || stashID == "" {
		t.Fatalf("StashPush: %q %v", stashID, err)
	}

	// One user stash, then cepm resolves its position, then a second user
	// stash shifts everything down again.
	if err := os.WriteFile(filepath.Join(dir, "one"), []byte("user one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "stash", "push", "-m", "user one")
	ref, err := repo.StashRef(ctx, stashID)
	if err != nil || ref == "" {
		t.Fatalf("StashRef: %q %v", ref, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "two"), []byte("user two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "stash", "push", "-m", "user two")

	if _, err := repo.StashPop(ctx, stashID); err != nil {
		t.Fatalf("popping our own stash after the shift: %v", err)
	}

	ours, err := os.ReadFile(filepath.Join(dir, "ours"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ours) != "cepm stashed this\n" {
		t.Errorf("cepm's change was not restored, file is %q", ours)
	}
	for _, f := range []string{"one", "two"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "committed\n" {
			t.Errorf("a user stash was applied to the working tree: %s = %q", f, b)
		}
	}
	list := gitIn(t, dir, "stash", "list")
	for _, msg := range []string{"user one", "user two"} {
		if !strings.Contains(list, msg) {
			t.Errorf("the user's stash %q must survive, list is:\n%s", msg, list)
		}
	}
	// cepm's own entry stays by design; the user's must be untouched.
}

// A user stash pushed between cepm's own push and its look must not be
// adopted as cepm's: the entry is found by the nonce in its message, not by
// whatever sits on top afterwards. The seam is the only way to land inside
// that gap deterministically.
func TestStashPushIdentifiesItsOwnEntry(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--initial-branch=main")
	for _, f := range []string{"ours", "theirs"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("committed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", "base")

	repo := Repo{Dir: dir}
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "ours"), []byte("cepm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	AfterStashPush = func() {
		AfterStashPush = nil // once: the user stashes right after we do
		if err := os.WriteFile(filepath.Join(dir, "theirs"), []byte("user\n"), 0o644); err != nil {
			t.Error(err)
			return
		}
		gitIn(t, dir, "stash", "push", "-m", "the user's own stash")
	}
	t.Cleanup(func() { AfterStashPush = nil })

	stashID, _, err := repo.StashPush(ctx)
	if err != nil || stashID == "" {
		t.Fatalf("StashPush: %q %v", stashID, err)
	}
	if subject := gitIn(t, dir, "show", "-s", "--format=%s", stashID); !strings.Contains(subject, "cepm auto-stash") {
		t.Fatalf("the recorded id names %q, not cepm's own entry", subject)
	}

	// And restoring it leaves the user's stash untouched.
	if _, err := repo.StashPop(ctx, stashID); err != nil {
		t.Fatalf("StashPop: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ours")); string(b) != "cepm\n" {
		t.Errorf("cepm's change was not restored: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "theirs")); string(b) != "committed\n" {
		t.Errorf("the user's stash was applied: %q", b)
	}
	if list := gitIn(t, dir, "stash", "list"); !strings.Contains(list, "the user's own stash") {
		t.Errorf("the user's stash must survive:\n%s", list)
	}
}

// cepm never removes a stash entry. git can only drop one by position, and
// a push landing between resolving that position and using it makes the drop
// take someone else's — and compensating afterwards leaves a window where
// their work exists nowhere but cepm's memory. So no user stash can be lost
// here, whatever anyone else is doing at the same time, and cepm's own entry
// is reported rather than quietly left.
func TestStashPopNeverRemovesAnyStash(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--initial-branch=main")
	for _, f := range []string{"ours", "one"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("committed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", "base")

	repo := Repo{Dir: dir}
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "ours"), []byte("cepm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stashID, _, err := repo.StashPush(ctx)
	if err != nil || stashID == "" {
		t.Fatalf("StashPush: %q %v", stashID, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "one"), []byte("user one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "stash", "push", "-m", "user one", "--", "one")

	before := gitIn(t, dir, "stash", "list", "--format=%H")
	leftBehind, err := repo.StashPop(ctx, stashID)
	if err != nil {
		t.Fatalf("StashPop: %v", err)
	}
	if leftBehind != stashID {
		t.Errorf("cepm's remaining entry must be reported, got %q", leftBehind)
	}
	if after := gitIn(t, dir, "stash", "list", "--format=%H"); after != before {
		t.Errorf("no stash entry may be removed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ours")); string(b) != "cepm\n" {
		t.Errorf("cepm's change was not restored: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "one")); string(b) != "committed\n" {
		t.Errorf("the user's stash was applied to the working tree: %q", b)
	}
}

// An entry the user dropped themselves is not reported as left behind —
// there is nothing for them to clean up.
func TestStashPopReportsNothingWhenTheEntryIsGone(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", "base")

	repo := Repo{Dir: dir}
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stashID, _, err := repo.StashPush(ctx)
	if err != nil || stashID == "" {
		t.Fatalf("StashPush: %q %v", stashID, err)
	}
	gitIn(t, dir, "stash", "drop")

	leftBehind, err := repo.StashPop(ctx, stashID)
	if err != nil {
		t.Fatalf("StashPop: %v", err)
	}
	if leftBehind != "" {
		t.Errorf("an entry that is already gone must not be reported as left behind, got %q", leftBehind)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f")); string(b) != "edited\n" {
		t.Errorf("the change was not restored: %q", b)
	}
}
