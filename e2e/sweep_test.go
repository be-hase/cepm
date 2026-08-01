//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The work root sits at a fixed name in world-writable /tmp, so it can be a
// symlink someone else planted; following it would sweep — and delete —
// wherever it points.
func TestWorkRootRefusesSymlinksAndForeignDirectories(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkRoot(link); err == nil {
		t.Error("a symlinked work root must be refused")
	}

	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkRoot(file); err == nil {
		t.Error("a non-directory work root must be refused")
	}

	// A pre-existing directory of this user gets its permissions tightened.
	loose := filepath.Join(base, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureWorkRoot(loose); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(loose); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("an existing work root should end up 0700, got %v %v", info.Mode(), err)
	}
}

// Signal(0) to another user's process fails with EPERM, which proves the pid
// exists. Treating that as "dead" would delete a work directory out from
// under a running suite.
func TestProcessAliveTreatsEPERMAsAlive(t *testing.T) {
	// pid 1 always exists; unless the tests run as root, signalling it is
	// EPERM — exactly the case that must count as alive.
	if !processAlive(1) {
		t.Error("pid 1 exists; EPERM from Signal(0) must count as alive")
	}
}

// The Go module cache is written read-only, and an entry can only be removed
// by writing to its parent: a plain RemoveAll leaves the whole tree behind,
// once per run, ~15MB at a time.
func TestWorkDirRemovalSurvivesReadOnlyDirectories(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "go", "pkg", "mod", "example.com", "dep@v1.0.0")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module x\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(nested), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Dir(nested), 0o755)
		_ = os.Chmod(nested, 0o755)
	})

	removeWorkDir(t, dir)

	if _, err := os.Stat(dir); err == nil {
		t.Error("a read-only module cache must not keep the work directory alive")
	}
}

// Sweeping on a name prefix and an age would eventually delete something a
// person put there, and would also hit a run that has simply been going for a
// while. Only a directory this suite can prove it abandoned may go.
func TestSweepOnlyRemovesProvenLeftovers(t *testing.T) {
	if err := ensureWorkRoot(workRoot); err != nil {
		t.Fatal(err)
	}

	// Something a person might have left in the same place.
	notOurs := filepath.Join(workRoot, "run-someones-notes")
	if err := os.MkdirAll(notOurs, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory claimed by a process that is still running (this one).
	alive := filepath.Join(workRoot, "run-alive")
	if err := os.MkdirAll(alive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alive, markerName),
		[]byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	// A genuine leftover: claimed by a pid that cannot be running.
	dead := filepath.Join(workRoot, "run-dead")
	if err := os.MkdirAll(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dead, markerName), []byte("99999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.RemoveAll(notOurs)
		os.RemoveAll(alive)
		os.RemoveAll(dead)
	})

	sweepStaleWorkDirs(t)

	if _, err := os.Stat(notOurs); err != nil {
		t.Errorf("a directory without an ownership marker must be left alone: %v", err)
	}
	if _, err := os.Stat(alive); err != nil {
		t.Errorf("a directory owned by a live process must be left alone: %v", err)
	}
	if _, err := os.Stat(dead); err == nil {
		t.Error("a directory owned by a dead process should have been removed")
	}
}
