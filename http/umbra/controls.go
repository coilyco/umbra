package umbra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/coilyco/umbra/cli/execverb"
	"github.com/coilyco/umbra/http/specverb"
	"github.com/coilyco/umbra/pkg/negcontrol"
)

// ErrControlsFailed is returned when a stated refusal does not hold. The report
// names every such rule rather than stopping at the first.
var ErrControlsFailed = errors.New("umbra: negative controls failed")

// ControlResult is one member's controls, keyed by its path in the project.
type ControlResult struct {
	Member   string
	Controls []negcontrol.Control
}

// Controls runs the negative controls for every exec and spec member of the
// project. MCP members carry no refusal rule to control. See docs/negative-controls.md.
func Controls(ctx context.Context, opts Options) ([]ControlResult, error) {
	g, err := loadGroup(opts)
	if err != nil {
		return nil, err
	}
	var out []ControlResult
	for _, m := range g.Members {
		var cs []negcontrol.Control
		switch {
		case m.isMCP():
			continue
		case m.isExec():
			cs, err = execverb.NegativeControls(ctx, m.ExecGF)
		default:
			var spec []byte
			if spec, err = readSpecLock(g.Dir, m); err != nil {
				return nil, fmt.Errorf("umbra: read spec lock for %s: %w", m.Path, err)
			}
			cs, err = specverb.NegativeControls(ctx, m.GF, spec)
		}
		if err != nil {
			return nil, fmt.Errorf("umbra: negative controls for %s: %w", m.Path, err)
		}
		out = append(out, ControlResult{Member: m.Path, Controls: cs})
	}
	return out, nil
}

// WriteControls prints one line per rule, members with none included, and
// returns ErrControlsFailed when any rule does not hold.
func WriteControls(w io.Writer, results []ControlResult) error {
	var all []negcontrol.Control
	for _, r := range results {
		if len(r.Controls) == 0 {
			_, _ = fmt.Fprintf(w, "%s: no never or withhold rules\n", r.Member)
		}
		for _, c := range r.Controls {
			mark := "holds"
			if !c.Holds() {
				mark = "FAILS"
			}
			_, _ = fmt.Fprintf(w, "%s  %s: %s %q  (%s)\n", mark, r.Member, c.Kind, c.Rule, strings.Join(c.Argv, " "))
		}
		all = append(all, r.Controls...)
	}
	held, failed := negcontrol.Summarize(all)
	_, _ = fmt.Fprintf(w, "\n%d of %d rules hold\n", held, len(all))
	for _, f := range failed {
		_, _ = fmt.Fprintf(w, "  %s\n", f)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w: %d of %d rules do not hold", ErrControlsFailed, len(failed), len(all))
	}
	return nil
}
