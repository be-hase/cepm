// Package testgit builds the environment the real-git tests run under.
//
// Tests that invoke git must not inherit the developer's configuration: a
// commit.gpgsign, a core.hooksPath, a url rewrite or an injected
// GIT_CONFIG_KEY_* makes them fail for reasons that have nothing to do with
// the code under test — and pass for the wrong reasons just as easily.
package testgit

import (
	"os"
	"strings"
)

// Env returns the process environment with every GIT_* variable removed —
// config files, config injection, and stray GIT_DIR/GIT_WORK_TREE alike —
// plus a pinned identity and the two settings that shut the global and
// system config files out. PATH and everything else git needs survive.
func Env(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=cepm test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=cepm test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	return append(env, extra...)
}
