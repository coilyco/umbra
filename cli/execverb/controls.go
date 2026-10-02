package execverb

import (
	"context"
	"errors"

	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/negcontrol"
	"github.com/urfave/cli/v3"
)

// NegativeControls invokes every `never run` and `withhold` in gf, spawning
// nothing, then again with each removed. See docs/negative-controls.md.
func NegativeControls(ctx context.Context, gf *Guardfile) ([]negcontrol.Control, error) {
	var out []negcontrol.Control
	for i, r := range gf.NeverRules {
		want := r.refusal().Error()
		without := *gf
		without.NeverRules = append(append([]NeverRule{}, gf.NeverRules[:i]...), gf.NeverRules[i+1:]...)
		c, err := control(ctx, gf, &without, "never", r.Label(), r.Subcommand, want)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	for i, w := range gf.Withheld {
		want := withheldAction(w)(ctx, nil).Error()
		without := *gf
		without.Withheld = append(append([]WithheldStub{}, gf.Withheld[:i]...), gf.Withheld[i+1:]...)
		c, err := control(ctx, gf, &without, "withhold", w.Label(), w.Subcommand, want)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func control(ctx context.Context, gf, without *Guardfile, kind, label string, sub []string, want string) (negcontrol.Control, error) {
	argv := append(append([]string{}, gf.Group...), sub...)
	got, err := invoke(ctx, gf, argv)
	if err != nil {
		return negcontrol.Control{}, err
	}
	// A rule whose removal leaves the guardfile invalid is load-bearing by
	// construction, and the mount error is the observed difference.
	gone, err := invoke(ctx, without, argv)
	if err != nil {
		gone = negcontrol.Outcome{Code: -1, Text: "mount refused: " + err.Error()}
	}
	return negcontrol.Judge(kind, label, argv, got, gone, want, exitcode.PolicyDenied), nil
}

// invoke builds gf the way the generated binary does and runs argv. A `shell`
// selector fails closed rather than executing on the host running the check.
func invoke(ctx context.Context, gf *Guardfile, argv []string) (negcontrol.Outcome, error) {
	var o negcontrol.Outcome
	cfg := Config{
		Guardfile: gf,
		Run: func(context.Context, string, []string, []string) error {
			o.Spawned = true
			return nil
		},
		Host: func(context.Context, []string) (string, error) {
			return "", errors.New("negative controls do not run host selectors")
		},
	}
	root, args, unmatched, err := controlTree(cfg, argv)
	if err != nil {
		return o, err
	}
	err = negcontrol.Run(ctx, root, args)
	if err == nil {
		err = *unmatched
	}
	if err != nil {
		o.Text = err.Error()
		o.Code = exitcode.Of(err)
	}
	return o, nil
}

// controlTree returns the command tree, the argv to run it with, and where an
// unmatched name's outcome lands (CommandNotFound returns nothing to Run).
func controlTree(cfg Config, argv []string) (*cli.Command, []string, *error, error) {
	gf := cfg.Guardfile
	unmatched := new(error)
	if !gf.Replace {
		root := &cli.Command{Name: gf.Group[0]}
		return root, argv, unmatched, Mount(root, cfg)
	}
	root, err := BuildReplacement(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	// A replacement is invoked as the tool it occludes, so argv drops the wrap path.
	args := append([]string{gf.Occlude}, argv[len(gf.Group):]...)
	fb, err := NewFallback(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	if fb.Open() {
		installForward(root, fb, func() []string { return args[1:] }, func(e error) { *unmatched = e })
	} else {
		installUnmatched(root, func(_ context.Context, _ *cli.Command, name string) {
			*unmatched = RefuseUngranted(gf, name)
		})
	}
	return root, args, unmatched, nil
}
