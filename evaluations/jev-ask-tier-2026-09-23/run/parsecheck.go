//go:build ignore

// parsecheck parses each guardfile path through umbra's own loader, so the
// polarity-flip B is proven to parse (PREREGISTER.md), not assumed.
package main

import (
	"fmt"
	"os"

	"github.com/coilyco/umbra/cli/execverb"
)

func main() {
	bad := 0
	for _, p := range os.Args[1:] {
		src, err := os.ReadFile(p)
		if err == nil {
			_, err = execverb.Parse(src)
		}
		if err != nil {
			bad++
			fmt.Printf("FAIL %s: %v\n", p, err)
		}
	}
	fmt.Printf("parsed %d, failed %d\n", len(os.Args)-1, bad)
	if bad > 0 {
		os.Exit(1)
	}
}
