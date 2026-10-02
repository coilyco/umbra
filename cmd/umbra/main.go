// Command umbra is the no-code driver for generating guarded consumer CLIs.
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
