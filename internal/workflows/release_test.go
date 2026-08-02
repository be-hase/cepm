package workflows

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func releaseYAML(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tagPattern extracts the regex the semver job feeds to grep -Eq. Testing
// the pattern actually shipped — not a copy — is the point: a drifted copy
// would pin nothing.
func tagPattern(t *testing.T, yml string) *regexp.Regexp {
	t.Helper()
	m := regexp.MustCompile(`grep -Eq '([^']+)'`).FindStringSubmatch(yml)
	if m == nil {
		t.Fatal("release.yml no longer contains a grep -Eq '<pattern>' tag check")
	}
	re, err := regexp.Compile(m[1])
	if err != nil {
		t.Fatalf("the tag pattern does not compile: %v", err)
	}
	return re
}

// GoReleaser derives the release version from the tag; anything that is not
// strict SemVer 2.0.0 (with the v prefix) fails late and messily, so the
// workflow's own pattern must accept exactly the valid shapes.
func TestReleaseTagPatternIsStrictSemver(t *testing.T) {
	re := tagPattern(t, releaseYAML(t))

	accept := []string{
		"v0.1.0",
		"v1.2.3",
		"v10.20.30",
		"v1.2.3-rc.1",
		"v1.2.3-alpha.1.beta-2",
		"v1.2.3-0.3.7",
		"v1.2.3+build.5",
		"v1.2.3-rc.1+sha.5114f85",
	}
	reject := []string{
		"v01.2.3",         // leading zero in core version
		"v1.02.3",         // leading zero in core version
		"v1.2.3-.",        // empty prerelease identifier
		"v1.2.3-alpha..1", // consecutive dots
		"v1.2.3+.",        // empty build identifier
		"v1.2.3-01",       // leading zero in numeric prerelease identifier
		"v1.2",            // incomplete core
		"v1.2.3.4",        // too many components
		"1.2.3",           // missing v prefix
		"release-1",
		"v1.2.3-",
		"v1.2.3+",
	}
	for _, tag := range accept {
		if !re.MatchString(tag) {
			t.Errorf("pattern must accept %s", tag)
		}
	}
	for _, tag := range reject {
		if re.MatchString(tag) {
			t.Errorf("pattern must reject %s", tag)
		}
	}
}

// The release toolchain must be reproducible: the same tag has to build
// with the same GoReleaser, so the workflow pins an exact version — a range
// like "~> v2" resolves to whatever is newest at run time.
func TestGoReleaserVersionIsPinnedExactly(t *testing.T) {
	yml := releaseYAML(t)
	m := regexp.MustCompile(`(?m)^\s+version: (\S+)$`).FindStringSubmatch(yml)
	if m == nil {
		t.Fatal("release.yml no longer sets a GoReleaser version")
	}
	if !regexp.MustCompile(`^v2\.[0-9]+\.[0-9]+$`).MatchString(m[1]) {
		t.Errorf("GoReleaser version %q is not an exact v2.x.y pin", m[1])
	}
}

// The job graph is load-bearing: verify must wait for the cheap tag check
// (a malformed tag must not spend two OSes' worth of verification minutes),
// and release must require both — that ordering is the whole release gate.
func TestReleaseJobDependencies(t *testing.T) {
	yml := releaseYAML(t)

	jobBlock := func(name string) string {
		t.Helper()
		start := strings.Index(yml, "\n  "+name+":")
		if start < 0 {
			t.Fatalf("job %q missing from release.yml", name)
		}
		rest := yml[start+1:]
		if end := regexp.MustCompile(`\n  [a-z]+:`).FindStringIndex(rest); end != nil {
			return rest[:end[0]]
		}
		return rest
	}

	if !strings.Contains(jobBlock("verify"), "needs: semver") {
		t.Error("verify must depend on the semver check")
	}
	release := jobBlock("release")
	if !regexp.MustCompile(`needs:\s*\[semver, verify\]`).MatchString(release) {
		t.Error("release must need both semver and verify")
	}
	if !strings.Contains(release, "contents: write") {
		t.Error("release must be the job holding contents: write")
	}
	// And the workflow default must stay read-only.
	head := yml[:strings.Index(yml, "jobs:")]
	if !strings.Contains(head, "contents: read") {
		t.Error("the workflow default permission must be contents: read")
	}
}

func readRepoFile(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, rel...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The gates only hold if CI actually runs them. Build-tagged code is
// invisible to an untagged vet or staticcheck, and the e2e harness has
// concurrency of its own — a data race hid there before — so the tagged
// checks and the race-enabled e2e are pinned here rather than trusted to
// survive an edit.
func TestCIRunsTheCheckedGates(t *testing.T) {
	ci := readRepoFile(t, ".github", "workflows", "ci.yml")
	for _, want := range []string{
		"go vet ./...",
		"go vet -tags e2e ./...",
		"go tool staticcheck ./...",
		"go tool staticcheck -tags e2e ./...",
		"go test -race ./...",
		"make e2e",
	} {
		if !strings.Contains(ci, want) {
			t.Errorf("ci.yml no longer runs %q", want)
		}
	}

	// And make e2e is what CI invokes, so the race detector has to live in
	// the target itself.
	mk := readRepoFile(t, "Makefile")
	i := strings.Index(mk, "\ne2e:")
	if i < 0 {
		t.Fatal("the Makefile no longer has an e2e target")
	}
	target := mk[i:]
	if end := strings.Index(target[1:], "\n\n"); end >= 0 {
		target = target[:end+1]
	}
	if !strings.Contains(target, "-race") {
		t.Errorf("make e2e must run the harness under -race, got:%s", target)
	}
	if !strings.Contains(target, "-tags e2e") {
		t.Errorf("make e2e must build with the e2e tag, got:%s", target)
	}
}
