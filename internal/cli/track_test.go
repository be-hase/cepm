package cli

import (
	"strings"
	"testing"
)

// A flag that belongs to the other mode is a misunderstanding about what is
// being configured; silently ignoring it would let "track tag --branch dev"
// read as if the branch mattered.
func TestTrackRejectsFlagsOfTheOtherMode(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"track", "tools", "release"}, `"branch" or "tag"`},
		{[]string{"track", "tools", "tag", "--branch", "main"}, "--branch only applies"},
		{[]string{"track", "tools", "branch", "--tag-pattern", "v*"}, "only apply to tag tracking"},
		{[]string{"track", "tools", "branch", "--prerelease"}, "only apply to tag tracking"},
		{[]string{"track", "tools", "branch", "--prerelease=false"}, "only apply to tag tracking"},
	}
	for _, c := range cases {
		_, err := run(t, "", c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want an error containing %q", c.args, err, c.want)
		}
	}
}

func TestTrackUnknownRepo(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	_, err := run(t, "", "track", "tools", "tag")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("got %v, want an unknown-repo error", err)
	}
}
