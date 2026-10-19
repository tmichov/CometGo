// Command cometgo is CometGo's developer tool. Today it scaffolds React
// Native apps (docs/mobile.md); `run` and `build` follow (M6).
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `cometgo: CometGo's developer tool.

Usage:
  cometgo mobile init [flags] [dir]   create an Expo app wired to your server (default dir: mobile)

Run "cometgo mobile init -h" for its flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches args and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 2 && args[0] == "mobile" && args[1] == "init" {
		if err := mobileInit(args[2:], stdout, stderr); err != nil {
			fmt.Fprintln(stderr, "cometgo mobile init:", err)
			return 1
		}
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}
