package main

import (
	"fmt"
	"os"

	"github.com/be-hase/cepm/internal/cli"
	"github.com/be-hase/cepm/internal/term"
)

func main() {
	if err := cli.Execute(); err != nil {
		// Errors can embed remote-controlled text (git stderr, manifest
		// values); the sources escape it, this is the backstop.
		fmt.Fprintln(os.Stderr, "Error:", term.SafeLines(err.Error()))
		os.Exit(1)
	}
}
