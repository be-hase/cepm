package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fingerprintRepo builds a committed repository and returns it with a
// fingerprint function that fails the test on error: every case here is a
// healthy tree, and for a healthy tree "could not fingerprint" is itself
// the bug — the caller treats it as unverifiable and asks a scarier
// question than the situation deserves.
func fingerprintRepo(t *testing.T) (dir string, fp func() string) {
	t.Helper()
	dir = t.TempDir()
	gitIn(t, dir, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", "base")
	repo := Repo{Dir: dir}
	return dir, func() string {
		got, err := repo.ChangeFingerprint(context.Background())
		if err != nil {
			t.Fatalf("ChangeFingerprint on a healthy tree: %v", err)
		}
		return got
	}
}

// Every case is a pair of states "git status" prints identically; the
// fingerprint has to tell them apart, because the user's approval covered
// the first and the delete would take the second.
func TestFingerprintSeesWhatStatusCannot(t *testing.T) {
	t.Run("untracked symlink retargeted", func(t *testing.T) {
		dir, fp := fingerprintRepo(t)
		link := filepath.Join(dir, "link")
		// Dangling on purpose: a healthy tree may hold one, and it must
		// fingerprint rather than fail into the unverifiable path.
		if err := os.Symlink("missing-one", link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		one := fp()
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("missing-two", link); err != nil {
			t.Fatal(err)
		}
		if fp() == one {
			t.Error("retargeting a symlink must change the fingerprint")
		}
	})

	t.Run("untracked name starting with a space", func(t *testing.T) {
		dir, fp := fingerprintRepo(t)
		odd := filepath.Join(dir, " leading-space.txt")
		if err := os.WriteFile(odd, []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		one := fp()
		if err := os.WriteFile(odd, []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if fp() == one {
			t.Error("rewriting a leading-space file must change the fingerprint")
		}
	})

	t.Run("untracked file made executable", func(t *testing.T) {
		dir, fp := fingerprintRepo(t)
		script := filepath.Join(dir, "run.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		one := fp()
		if err := os.Chmod(script, 0o755); err != nil {
			t.Fatal(err)
		}
		if fp() == one {
			t.Error("chmod +x must change the fingerprint")
		}
	})

	t.Run("content shifted across file boundaries", func(t *testing.T) {
		dir, fp := fingerprintRepo(t)
		a, b := filepath.Join(dir, "a.bin"), filepath.Join(dir, "b.bin")
		if err := os.WriteFile(a, []byte("x\x00y"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b, []byte("z"), 0o644); err != nil {
			t.Fatal(err)
		}
		one := fp()
		if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b, []byte("y\x00z"), 0o644); err != nil {
			t.Fatal(err)
		}
		if fp() == one {
			t.Error("moving bytes between files must change the fingerprint")
		}
		// Honest scope: this is the shape most sensitive to framing
		// regressions, not proof that unframed concatenation would
		// collide — the path and mode records between contents make a
		// real collision hard to construct. The length prefixes exist to
		// rule the ambiguity out structurally rather than case by case.
	})

	t.Run("untracked file inside a submodule rewritten", func(t *testing.T) {
		sub := t.TempDir()
		gitIn(t, sub, "init", "--initial-branch=main")
		if err := os.WriteFile(filepath.Join(sub, "file.txt"), []byte("committed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, sub, "add", "-A")
		gitIn(t, sub, "commit", "-m", "sub base")

		dir, fp := fingerprintRepo(t)
		gitIn(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", sub, "vendor")
		gitIn(t, dir, "commit", "-m", "add submodule")

		// Untracked inside the submodule: the parent's diff is empty and
		// its untracked listing does not descend, so only recursing into
		// the submodule can see this content at all.
		inner := filepath.Join(dir, "vendor", "local.txt")
		if err := os.WriteFile(inner, []byte("edit-one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		one := fp()
		if err := os.WriteFile(inner, []byte("edit-two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if fp() == one {
			t.Error("rewriting an untracked file inside a submodule must change the fingerprint")
		}
	})

	t.Run("external diff driver hiding the change", func(t *testing.T) {
		dir, fp := fingerprintRepo(t)
		// A driver that reports nothing, wired exactly as a repository (or
		// the user's config) could wire it. The fingerprint needs what the
		// tree is, not what a driver chooses to show.
		if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt diff=opaque\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "config", "diff.opaque.command", "true")
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "commit", "-m", "attributes")

		file := filepath.Join(dir, "base.txt")
		if err := os.WriteFile(file, []byte("edit-one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		one := fp()
		if err := os.WriteFile(file, []byte("edit-two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if fp() == one {
			t.Error("an external diff driver must not decide what the fingerprint sees")
		}
	})

	t.Run("rewrite inside a tracked submodule", func(t *testing.T) {
		sub := t.TempDir()
		gitIn(t, sub, "init", "--initial-branch=main")
		if err := os.WriteFile(filepath.Join(sub, "file.txt"), []byte("committed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, sub, "add", "-A")
		gitIn(t, sub, "commit", "-m", "sub base")

		dir, fp := fingerprintRepo(t)
		gitIn(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", sub, "vendor")
		gitIn(t, dir, "commit", "-m", "add submodule")

		inner := filepath.Join(dir, "vendor", "file.txt")
		if err := os.WriteFile(inner, []byte("edit-one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		one := fp()
		if err := os.WriteFile(inner, []byte("edit-two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if fp() == one {
			t.Error("a rewrite inside a submodule must change the fingerprint: " +
				"without --submodule=diff both states print the same '-dirty' line")
		}
	})
}
