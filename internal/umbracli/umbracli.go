// Package umbracli owns the shared command tree behind the canonical umbra
// binary and its temporary kdl-specs compatibility entrypoint.
package umbracli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/coilyco/umbra/http/umbra"
	"github.com/urfave/cli/v3"
)

// Run executes the umbra command tree and returns its process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := app()
	cmd.Writer = stdout
	cmd.ErrWriter = stderr
	if err := cmd.Run(ctx, args); err != nil {
		_, _ = fmt.Fprintln(stderr, "umbra:", err)
		return exitCode(err)
	}
	return 0
}

// exitCode maps a driver error to a process exit code, so skew drift is
// distinguishable from an offline fetch or any other failure.
func exitCode(err error) int {
	if errors.Is(err, umbra.ErrSkew) {
		return 3
	}
	return 1
}

// app builds the driver command tree. Shared discovery flags are persistent so
// every verb reads the same project boundary and optional member selector.
func app() *cli.Command {
	return &cli.Command{
		Name:    "umbra",
		Usage:   "no-code driver for a spec-driven consumer CLI (gen / lock / skew / build / run / install / doctor)",
		Version: fmt.Sprintf("%s (umbra ref %s)", umbra.DriverVersion(), umbra.DefaultCLIGuardRef()),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "guardfile",
				Usage: "operation KDL member selecting its wrap binary",
			},
			&cli.StringFlag{
				Name:  "project-root",
				Usage: "recursive KDL discovery root (members are identified by parsed wrap declarations)",
			},
			&cli.StringFlag{
				Name:  "skills-out",
				Usage: "explicit skill root; writes <root>/<binary>/SKILL.md and references/commands.yaml",
			},
			// --out on the root keeps the legacy one-shot signature working.
			// Local so it does not collide with gen's own --out on subcommands.
			&cli.StringFlag{Name: "out", Hidden: true, Local: true},
			&cli.StringFlag{Name: "binary", Hidden: true, Local: true},
		},
		Commands: []*cli.Command{
			genCmd(),
			lockCmd(),
			skewCmd(),
			buildCmd(),
			runCmd(),
			openapiCmd(),
			installCmd(),
			doctorCmd(),
			controlsCmd(),
		},
		// Root action keeps the legacy `--guardfile X --out Y` one-shot working:
		// with --out set and no subcommand, behave as `gen --out Y`.
		Action: func(_ context.Context, c *cli.Command) error {
			if c.String("out") == "" {
				return cli.ShowAppHelp(c)
			}
			return umbra.Gen(options(c, c.String("out"), c.String("binary")))
		},
	}
}

func binaryNameFlag() cli.Flag {
	return &cli.StringFlag{Name: "binary", Usage: "generated CLI/binary name (default: Guardfile wrap binary)"}
}

func genCmd() *cli.Command {
	return &cli.Command{
		Name:  "gen",
		Usage: "render the consumer main.go (into the cache, or --out for inspection)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "out", Usage: "write main.go here instead of the cache (debug)"},
			binaryNameFlag(),
		},
		Action: func(_ context.Context, c *cli.Command) error {
			return umbra.Gen(options(c, c.String("out"), c.String("binary")))
		},
	}
}

// openapiCmd emits a spec describing what the guardfile grants, which is a
// narrowing of the upstream rather than a new contract.
func openapiCmd() *cli.Command {
	return &cli.Command{
		Name:  "openapi",
		Usage: "emit an OpenAPI 3.1 document for the granted surface (stdout, or --out)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "out", Usage: "write the document here instead of stdout"},
			&cli.StringFlag{Name: "doc-version", Usage: "info.version for the emitted document", Value: "0.0.0"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			return umbra.OpenAPI(options(c, c.String("out"), ""), c.String("doc-version"))
		},
	}
}

func lockCmd() *cli.Command {
	return &cli.Command{
		Name:  "lock",
		Usage: "fetch the upstream spec and freeze the build (writes the spec lock + specverb.lock)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "umbra-ref", Usage: "umbra module version/commit to pin (default: the driver's own version, else latest)"},
			&cli.StringFlag{Name: "umbra-replace", Usage: "local umbra checkout to build against (dev locks only)"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			return umbra.Lock(umbra.Options{
				GuardfilePath:   resolveGuardfile(c),
				ProjectRoot:     c.String("project-root"),
				CLIGuardRef:     c.String("umbra-ref"),
				CLIGuardReplace: c.String("umbra-replace"),
				SkillsOut:       c.String("skills-out"),
			})
		},
	}
}

func skewCmd() *cli.Command {
	return &cli.Command{
		Name:  "skew",
		Usage: "report drift between the committed spec lock and live upstream (exit 3 on drift)",
		Action: func(_ context.Context, c *cli.Command) error {
			return umbra.Skew(options(c, "", ""))
		},
	}
}

func buildCmd() *cli.Command {
	return &cli.Command{
		Name:  "build",
		Usage: "materialize+build the consumer binary if stale, then write it to --out (a dir or file path)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "out", Value: "bin", Usage: "output directory (binary keeps its generated name) or explicit file path"},
			binaryNameFlag(),
			&cli.StringFlag{Name: "set-version", Usage: "release version stamped into the binary's --version via -ldflags (default \"dev\")"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			return umbra.Build(umbra.Options{
				GuardfilePath: resolveGuardfile(c),
				ProjectRoot:   c.String("project-root"),
				BinaryName:    c.String("binary"),
				Out:           c.String("out"),
				Version:       c.String("set-version"),
				SkillsOut:     c.String("skills-out"),
			})
		},
	}
}

// shimDirFlag names the PATH directory an occluded replacement is installed
// onto. It is not an output path: the filename is the occluded tool's own name.
func shimDirFlag() cli.Flag {
	return &cli.StringFlag{Name: "shim-dir", Usage: "PATH directory the replacement is installed onto (its filename is the occluded tool's name)"}
}

// installCmd is `build` for a replacement, with the destination filename fixed
// by the guardfile rather than by a flag.
func installCmd() *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "build an occluded replacement and place it on --shim-dir under the name it occludes",
		Flags: []cli.Flag{shimDirFlag(), &cli.StringFlag{Name: "set-version", Usage: "release version stamped into the binary via -ldflags (default \"dev\")"}},
		Action: func(_ context.Context, c *cli.Command) error {
			return umbra.Install(umbra.Options{
				GuardfilePath: resolveGuardfile(c),
				ProjectRoot:   c.String("project-root"),
				ShimDir:       c.String("shim-dir"),
				Version:       c.String("set-version"),
				SkillsOut:     c.String("skills-out"),
			})
		},
	}
}

// doctorCmd reports what a replacement installation achieves on this host, and
// changes nothing. Its last finding never passes: see docs/execverb-replacement.md.
func doctorCmd() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "report what an installed replacement occludes on this host (read-only)",
		Flags: []cli.Flag{shimDirFlag()},
		Action: func(_ context.Context, c *cli.Command) error {
			findings, err := umbra.Doctor(umbra.Options{
				GuardfilePath: resolveGuardfile(c),
				ProjectRoot:   c.String("project-root"),
				ShimDir:       c.String("shim-dir"),
			})
			if err != nil {
				return err
			}
			umbra.WriteFindings(c.Root().Writer, findings)
			return nil
		},
	}
}

// controlsCmd invokes every never and withhold rule and fails when one does not
// hold. See docs/negative-controls.md.
func controlsCmd() *cli.Command {
	return &cli.Command{
		Name:  "controls",
		Usage: "check that every never and withhold rule refuses, and that removing it changes the outcome (read-only)",
		Action: func(ctx context.Context, c *cli.Command) error {
			results, err := umbra.Controls(ctx, umbra.Options{
				GuardfilePath: resolveGuardfile(c),
				ProjectRoot:   c.String("project-root"),
			})
			if err != nil {
				return err
			}
			return umbra.WriteControls(c.Root().Writer, results)
		},
	}
}

func runCmd() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "materialize+build the consumer binary if stale, then exec it with the remaining args",
		Flags: []cli.Flag{
			binaryNameFlag(),
		},
		Action: func(_ context.Context, c *cli.Command) error {
			return umbra.Run(umbra.Options{
				GuardfilePath: resolveGuardfile(c),
				ProjectRoot:   c.String("project-root"),
				BinaryName:    c.String("binary"),
				Args:          c.Args().Slice(),
				SkillsOut:     c.String("skills-out"),
			})
		},
	}
}

// resolveGuardfile returns the --guardfile value verbatim (empty when unset).
// The driver (umbra.loadGroup) owns discovery and the merge-vs-error rules.
func resolveGuardfile(c *cli.Command) string {
	return c.String("guardfile")
}

func options(c *cli.Command, out, binary string) umbra.Options {
	return umbra.Options{
		GuardfilePath: resolveGuardfile(c),
		ProjectRoot:   c.String("project-root"),
		BinaryName:    binary,
		Out:           out,
		SkillsOut:     c.String("skills-out"),
	}
}
