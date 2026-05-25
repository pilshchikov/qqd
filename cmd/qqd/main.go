package main

import (
	"fmt"
	"os"

	"github.com/pilshchikov/qqd/internal/qqd"
)

// version info set by goreleaser via ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	qqd.SetBuildInfo(version, commit, date)
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("qqd %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	}
	if err := qqd.Execute(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
