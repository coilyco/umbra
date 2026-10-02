// Command kdl-specs is the compatibility entrypoint for consumers that have
// not migrated their Go invocation path to cmd/umbra.
package main

import (
	"context"
	"os"

	"github.com/coilyco/umbra/internal/umbracli"
)

func main() {
	if code := umbracli.Run(context.Background(), os.Args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}
