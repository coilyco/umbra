// Command gen-webdocs renders each umbra example's CLI tree as a
// static HTML site under ../../site/cli/<example>/ via cli-web-docs.
package main

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"

	"github.com/coilyco/umbra/internal/clitrees"
	webdocs "github.com/coilysiren/cli-web-docs"
	"github.com/coilysiren/cli-web-docs/layout"
	"github.com/urfave/cli/v3"
)

type entry struct {
	dir   string
	title string
	build func() *cli.Command
}

func main() {
	entries := []entry{
		{"audit", "umbra audit demo", func() *cli.Command { return clitrees.Audit(nil) }},
		{"exitcode", "umbra exit-code taxonomy", clitrees.Exitcode},
		{"policy", "umbra metacharacter gate", clitrees.Policy},
	}

	outRoot := "../../site/cli"
	for _, e := range entries {
		out := filepath.Join(outRoot, e.dir)
		if err := webdocs.Render(e.build(), webdocs.Options{
			OutputDir: out,
			Title:     e.title,
			PerPage:   true,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "gen-webdocs:", e.dir, err)
			os.Exit(1)
		}
		fmt.Println("rendered", out)
	}

	if err := writeIndex(outRoot, entries); err != nil {
		fmt.Fprintln(os.Stderr, "gen-webdocs: index:", err)
		os.Exit(1)
	}
}

func writeIndex(outRoot string, entries []entry) error {
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		return err
	}
	items := make([]layout.NavItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, layout.NavItem{
			Label:    e.dir,
			Href:     e.dir + "/",
			Subtitle: e.title,
		})
	}
	body := layout.BuildNavList(items)
	page := layout.Page{
		Title:    "umbra CLI reference",
		Subtitle: "Rendered command tree for every example.",
		Body:     template.HTML(`<p>Pick an example:</p>` + string(body)),
	}
	f, err := os.Create(filepath.Join(outRoot, "index.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	return layout.Render(f, page)
}
