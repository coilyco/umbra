// The `openapi` driver verb: emit an OpenAPI document from a guardfile's
// granted subset. See docs/openapigen.md.

package umbra

import (
	"fmt"
	"os"
	"sort"

	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/http/openapigen"
	"github.com/coilyco/umbra/http/specverb"
)

// OpenAPI writes an OpenAPI document describing the group's granted HTTP
// surface to opts.Out, or to stdout when Out is empty.
func OpenAPI(opts Options, docVersion string) error {
	g, err := loadGroup(opts)
	if err != nil {
		return err
	}
	descs, baseURL, skipped, err := groupDescriptors(g)
	if err != nil {
		return err
	}
	raw, unreachable, err := openapigen.Emit(descs, openapigen.Config{
		Title:   g.runtimeBinary(),
		Descr:   "The granted subset of this upstream, as declared by the guardfile.",
		Version: docVersion,
		BaseURL: baseURL,
	})
	if err != nil {
		return err
	}
	skipped = append(skipped, unreachable...)
	sort.Strings(skipped)
	for _, name := range skipped {
		fmt.Fprintf(os.Stderr, "umbra: openapi: skipped %s (reaches no URL)\n", name)
	}
	if opts.Out == "" {
		_, err = os.Stdout.Write(raw)
		return err
	}
	if err := os.WriteFile(opts.Out, raw, 0o600); err != nil {
		return fmt.Errorf("umbra: write %s: %w", opts.Out, err)
	}
	fmt.Fprintf(os.Stderr, "umbra: wrote %s\n", opts.Out)
	return nil
}

// groupDescriptors collects every spec member's descriptors. Exec and mcp
// members are named as skipped: neither addresses an HTTP path.
func groupDescriptors(g *group) ([]opcore.Descriptor, string, []string, error) {
	var all []opcore.Descriptor
	var skipped []string
	baseURL := ""
	for _, m := range g.Members {
		if m.isExec() || m.isMCP() {
			skipped = append(skipped, m.Path)
			continue
		}
		specBytes, err := readSpecLock(g.Dir, m)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, "", nil, fmt.Errorf("umbra: no spec lock %s for openapi output: %w", m.Params.SpecLockName, ErrNoLock)
			}
			return nil, "", nil, fmt.Errorf("umbra: read spec lock for openapi output: %w", err)
		}
		descs, rt, err := specverb.Descriptors(specverb.DescriptorConfig{Guardfile: m.GF, Spec: specBytes})
		if err != nil {
			return nil, "", nil, fmt.Errorf("umbra: resolve descriptors for %s: %w", m.Path, err)
		}
		if baseURL == "" {
			baseURL = rt.BaseURL
		}
		all = append(all, descs...)
	}
	return all, baseURL, skipped, nil
}
