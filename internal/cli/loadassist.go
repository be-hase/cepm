package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/term"
)

// loadTarget is one extension the user needs to "Load unpacked".
type loadTarget struct {
	Name   string
	AbsDir string
	ID     string
}

// stdinReader is shared by every prompt: a fresh bufio.Reader per prompt
// would swallow the answers to later prompts sitting in its read-ahead
// buffer (visible when input is piped).
var (
	stdinOnce   sync.Once
	stdinShared *bufio.Reader
)

func promptReader(cmd *cobra.Command) *bufio.Reader {
	stdinOnce.Do(func() { stdinShared = bufio.NewReader(cmd.InOrStdin()) })
	return stdinShared
}

// resetPromptReader lets tests run several commands in one process, each with
// its own stdin.
func resetPromptReader() {
	stdinOnce = sync.Once{}
	stdinShared = nil
}

// selectByNumbers shows a numbered list and returns the chosen indices.
// Empty input selects everything.
func selectByNumbers(cmd *cobra.Command, prompt string, items []string) ([]int, error) {
	out := cmd.OutOrStdout()
	for i, item := range items {
		fmt.Fprintf(out, "  [%d] %s\n", i+1, item)
	}
	fmt.Fprintf(out, "%s [Enter=all, or numbers like \"1,3\"]: ", prompt)
	line, err := promptReader(cmd).ReadString('\n')
	if err != nil && err != io.EOF {
		return nil, err
	}
	line = strings.TrimSpace(line)
	if line == "" || strings.EqualFold(line, "a") || strings.EqualFold(line, "all") {
		all := make([]int, len(items))
		for i := range items {
			all[i] = i
		}
		return all, nil
	}
	var picked []int
	seen := map[int]bool{}
	for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 1 || n > len(items) {
			return nil, fmt.Errorf("invalid selection %q (expected numbers 1-%d)", tok, len(items))
		}
		if !seen[n-1] {
			seen[n-1] = true
			picked = append(picked, n-1)
		}
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("nothing selected")
	}
	return picked, nil
}

// confirm asks a yes/no question, defaulting to no.
func confirm(cmd *cobra.Command, question string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N]: ", question)
	line, err := promptReader(cmd).ReadString('\n')
	if err != nil && err != io.EOF {
		return false
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

// runLoadCeremony walks the user through loading each target: path on the
// clipboard, chrome://extensions opened, and — when the host is reachable —
// a live confirmation that Chrome actually loaded the right directory.
func runLoadCeremony(ctx context.Context, cmd *cobra.Command, targets []loadTarget) {
	if len(targets) == 0 {
		return
	}
	out := cmd.OutOrStdout()
	interactive := assist.IsTTY()

	if !interactive {
		fmt.Fprintf(out, "\nOne-time step: open chrome://extensions, enable Developer mode,\n")
		fmt.Fprintf(out, "and use \"Load unpacked\" for:\n")
		for _, t := range targets {
			fmt.Fprintf(out, "  %-24s %s\n", t.Name, term.Quote(t.AbsDir))
		}
		return
	}

	pageOpened := assist.OpenExtensionsPage() == nil
	for i, t := range targets {
		fmt.Fprintf(out, "\nLoading steps for %q (%d/%d):\n", t.Name, i+1, len(targets))
		step := 1
		if pageOpened && i == 0 {
			fmt.Fprintf(out, "  %d. Opening chrome://extensions ... (opened)\n", step)
			step++
		} else if !pageOpened && i == 0 {
			fmt.Fprintf(out, "  %d. Open chrome://extensions\n", step)
			step++
		}
		if i == 0 {
			fmt.Fprintf(out, "  %d. Turn on \"Developer mode\" (top right) if you haven't\n", step)
			step++
		}
		copied := assist.CopyToClipboard(t.AbsDir) == nil
		if copied {
			fmt.Fprintf(out, "  %d. Click \"Load unpacked\", press ⌘⇧G, paste (path copied!), Enter\n", step)
		} else {
			fmt.Fprintf(out, "  %d. Click \"Load unpacked\" and select: %s\n", step, term.Quote(t.AbsDir))
		}

		// Other targets of this same ceremony are not "the wrong directory".
		expected := map[string]bool{helperext.ExtensionID(): true}
		for _, other := range targets {
			expected[other.ID] = true
		}
		switch waitForLoaded(ctx, out, t, expected) {
		case loadConfirmed:
			// message already printed
		case hostUnreachable:
			fmt.Fprintf(out, "  (Chrome/helper not reachable — verify later with: cepm list)\n")
		case loadTimedOut:
			fmt.Fprintf(out, "  Not confirmed yet — verify later with: cepm list\n")
		}
	}
}

type loadWaitResult int

const (
	loadConfirmed loadWaitResult = iota
	hostUnreachable
	loadTimedOut
)

const loadWaitTimeout = 90 * time.Second

// waitForLoaded polls Chrome (via the host) until the expected extension ID
// appears. It also notices when a *different* new unpacked extension shows up
// — usually the wrong directory was selected — and says so.
func waitForLoaded(ctx context.Context, out io.Writer, t loadTarget, expected map[string]bool) loadWaitResult {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	baselineList, err := ipc.ListChrome(probeCtx)
	cancel()
	if err != nil {
		return hostUnreachable
	}
	baseline := map[string]bool{}
	for _, e := range baselineList {
		if e.ID == t.ID {
			fmt.Fprintf(out, "  ✔ already loaded (id: %s)\n", t.ID)
			return loadConfirmed
		}
		baseline[e.ID] = true
	}

	fmt.Fprintf(out, "  Waiting for Chrome to load it ... ")
	warned := false
	deadline := time.Now().Add(loadWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out)
			return loadTimedOut
		case <-time.After(time.Second):
		}
		pollCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		exts, err := ipc.ListChrome(pollCtx)
		cancel()
		if err != nil {
			continue // transient; keep waiting
		}
		// Check for the expected ID across the whole list first: reporting a
		// mismatch when the right extension is also present would be wrong.
		for _, e := range exts {
			if e.ID == t.ID {
				fmt.Fprintf(out, "✔ loaded! (id: %s)\n", t.ID)
				return loadConfirmed
			}
		}
		for _, e := range exts {
			if !baseline[e.ID] && !expected[e.ID] && !warned {
				warned = true
				fmt.Fprintf(out, "\n  ⚠ %q was loaded, but not from the expected directory.\n", e.Name)
				fmt.Fprintf(out, "    Expected exactly: %s\n  Waiting ... ", t.AbsDir)
			}
		}
	}
	fmt.Fprintln(out)
	return loadTimedOut
}
