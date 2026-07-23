// Command kdl-specs is the no-code uv-style driver for a spec-driven
// consumer CLI: gen / lock / skew / build / run. See docs/kdl-specs.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"forgejo.coilysiren.me/coilyco-flight-deck/cli-guard/http/kdlspecs"
	"github.com/urfave/cli/v3"
)

func main() {
	if err := app().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "kdl-specs:", err)
		os.Exit(exitCode(err))
	}
}

// exitCode maps a driver error to a process exit code, so skew drift is
// distinguishable from an offline fetch or any other failure.
func exitCode(err error) int {
	if errors.Is(err, kdlspecs.ErrSkew) {
		return 3
	}
	return 1
}

// app builds the driver command tree. The shared --guardfile is a persistent
// root flag so every verb reads the same value.
func app() *cli.Command {
	return &cli.Command{
		Name:    "kdl-specs",
		Usage:   "no-code driver for a spec-driven consumer CLI (gen / lock / skew / build / run)",
		Version: fmt.Sprintf("%s (cli-guard ref %s)", kdlspecs.DriverVersion(), kdlspecs.DefaultCLIGuardRef()),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "guardfile",
				Usage: "path to the consumer's KDL Guardfile (default: the lone *.guardfile.kdl in cwd)",
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
		},
		// Root action keeps the legacy `--guardfile X --out Y` one-shot working:
		// with --out set and no subcommand, behave as `gen --out Y`.
		Action: func(_ context.Context, c *cli.Command) error {
			if c.String("out") == "" {
				return cli.ShowAppHelp(c)
			}
			gf := resolveGuardfile(c)
			return kdlspecs.Gen(kdlspecs.Options{GuardfilePath: gf, BinaryName: c.String("binary"), Out: c.String("out")})
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
			gf := resolveGuardfile(c)
			return kdlspecs.Gen(kdlspecs.Options{GuardfilePath: gf, BinaryName: c.String("binary"), Out: c.String("out")})
		},
	}
}

func lockCmd() *cli.Command {
	return &cli.Command{
		Name:  "lock",
		Usage: "fetch the upstream spec and freeze the build (writes the spec lock + specverb.lock)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "cli-guard-ref", Usage: "cli-guard module version/commit to pin (default: the driver's own version, else latest)"},
			&cli.StringFlag{Name: "cli-guard-replace", Usage: "local cli-guard checkout to build against (dev locks only)"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			gf := resolveGuardfile(c)
			return kdlspecs.Lock(kdlspecs.Options{
				GuardfilePath:   gf,
				CLIGuardRef:     c.String("cli-guard-ref"),
				CLIGuardReplace: c.String("cli-guard-replace"),
			})
		},
	}
}

func skewCmd() *cli.Command {
	return &cli.Command{
		Name:  "skew",
		Usage: "report drift between the committed spec lock and live upstream (exit 3 on drift)",
		Action: func(_ context.Context, c *cli.Command) error {
			gf := resolveGuardfile(c)
			return kdlspecs.Skew(kdlspecs.Options{GuardfilePath: gf})
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
			gf := resolveGuardfile(c)
			return kdlspecs.Build(kdlspecs.Options{
				GuardfilePath: gf,
				BinaryName:    c.String("binary"),
				Out:           c.String("out"),
				Version:       c.String("set-version"),
			})
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
			gf := resolveGuardfile(c)
			return kdlspecs.Run(kdlspecs.Options{GuardfilePath: gf, BinaryName: c.String("binary"), Args: c.Args().Slice()})
		},
	}
}

// resolveGuardfile returns the --guardfile value verbatim (empty when unset).
// The driver (kdlspecs.loadGroup) owns discovery and the merge-vs-error rules.
func resolveGuardfile(c *cli.Command) string {
	return c.String("guardfile")
}
