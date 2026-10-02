// Package clitrees exports the demo command trees scripts/gen-webdocs renders
// into site/cli/. Compiled by root CI, which is why it is not in that module.
package clitrees

import (
	"context"
	"errors"
	"fmt"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/pkg/audit"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/policy"
	"github.com/urfave/cli/v3"
)

// Audit is the audit-surface tree. writer may be nil for doc
// rendering since Actions are not executed.
func Audit(writer *audit.Writer) *cli.Command {
	return &cli.Command{
		Name:    "demo",
		Usage:   "tiny umbra demo app",
		Version: "v0.0.0",
		Description: `demo is the smallest end-to-end exercise of the umbra pipeline:

    1. The wrapped Action runs through policy.ValidateArg over every
       user-supplied string before execve.
    2. An append-only JSONL row lands in $TMPDIR/umbra-demo.jsonl
       with timestamp, argv, cwd, exit code, and the forensic RepoRoot
       (git toplevel of cwd, or empty outside any repo).

Why an append-only audit log: it is the forensic trail if an agent
(or a confused human) invokes something destructive. The log lives
outside the working tree, is written 0600 in a 0700 dir, and rotates
via lumberjack when the active file hits the size cap. Old backups
past the retention horizon are pruned. There is no "skip audit" knob.

Operating model for an agent calling these commands:

    - Failure to preflight the audit dir is a hard fail at startup,
      not per call. If you see "audit preflight: ..." on stderr, do
      not retry; the host is broken (disk full, perms wrong, dir not
      writable). Surface to the operator.
    - Inspect the audit row after the call to reconstruct what
      happened: ` + "`tail -1 \"$TMPDIR/umbra-demo.jsonl\" | jq`" + `.`,
		Commands: []*cli.Command{
			{
				Name:      "hello",
				Usage:     "print a greeting",
				ArgsUsage: "<name>",
				Description: `Audited greeting verb. Demonstrates verb.Wrap composing every
umbra primitive on a single Action.

Examples:

    # greet
    demo hello world
    # hello, world

    # empty name accepted - defaults to "world"
    demo hello
    # hello, world

    # rejected by policy.ValidateArg (shell metacharacter)
    demo hello 'world; whoami'
    # policy: shell metacharacter rejected: arg positional[0] contains ';' at index 5

The audit row produced on a passing call includes:

    {"ts":"2026-...","verb":"hello","argv":["hello","world"],"cwd":"...","repo_root":"...","exit":0}

On a rejected call the row records exit=2 and the policy reason in
the envelope. An agent reading the log can reconstruct intent without
parsing stderr.`,
				Action: verb.Wrap(verb.Spec{
					Name: "hello",
					ArgsFunc: func(c *cli.Command) (map[string]string, []string) {
						return nil, c.Args().Slice()
					},
					Action: func(_ context.Context, c *cli.Command) error {
						name := c.Args().First()
						if name == "" {
							name = "world"
						}
						fmt.Printf("hello, %s\n", name)
						return nil
					},
				}, writer),
			},
		},
	}
}

// Exitcode is the tree for the exit-code taxonomy demo.
func Exitcode() *cli.Command {
	return &cli.Command{
		Name:    "exitcode-demo",
		Usage:   "show the public exit-code taxonomy",
		Version: "v0.0.0",
		Description: `exitcode-demo exercises the public exit-code taxonomy. External
consumers (orchestrators, CI steps, watchdogs, retry loops) pattern-
match on the process exit code to decide retry vs. abort vs. handoff
without parsing stderr. The codes are a stable contract.

The taxonomy:

    0 - Success      verb ran, underlying tool returned without error
    1 - Generic      catch-all, prefer a typed code over this
    2 - PolicyDenied the security pre-flight rejected the call (metachar,
                     missing required arg, deny rule hit). The
                     underlying tool was never invoked.
    3 - UpstreamFailed the wrapped tool ran and returned non-zero.
                     Stdout/stderr from the tool flow through; the
                     envelope's message is the wrapping error.
    4 - Internal     consumer-internal failure (config load, manifest
                     miss, audit-write fail). Distinct from
                     PolicyDenied because the user cannot fix it;
                     this is a consumer bug or a host problem.
    5 - UserError    obviously-wrong input that wasn't a metachar
                     rejection (missing flag, wrong arg count).
                     Distinct from PolicyDenied so a consumer can
                     differentiate "you typed it wrong" from "policy
                     says no".

Operating model for an agent consuming an exit code:

    - 2 (PolicyDenied) is non-retryable. The argv is hostile or
      malformed in a way that the gate refuses to forward. Surface
      to operator; do not escape and retry.
    - 3 (UpstreamFailed) is potentially retryable but only with the
      same argv and only if the underlying tool's exit suggests
      transience (network blip, lock contention). The wrapper does
      not retry for you. Read stderr to decide.
    - 4 (Internal) is non-retryable on this host. Report a bug, try
      a different host, or wait for a fix.
    - 5 (UserError) is non-retryable from automation; the operator
      needs to correct the input.

Add a new code only when an external consumer can act differently on
it. Do not subdivide for taxonomy; a single rejection class with a
yaml error envelope is more useful than a fan-out of codes.`,
		Commands: []*cli.Command{
			{
				Name:  "success",
				Usage: "exit 0",
				Description: `Returns nil. Process exits 0.

Examples:

    exitcode-demo success ; echo "exit: $?"
    # exit: 0`,
				Action: func(_ context.Context, _ *cli.Command) error { return nil },
			},
			{
				Name:  "policy",
				Usage: "exit 2 (PolicyDenied)",
				Description: `Returns exitcode.New(PolicyDenied, ...). Process exits 2. The yaml
envelope on stderr is the structured form an orchestrator parses.

Examples:

    exitcode-demo policy ; echo "exit: $?"
    # error:
    #     kind: policy
    #     message: argv rejected
    #     hint: fix the input
    # exit: 2

Treat exit 2 as authoritative-non-retryable.`,
				Action: func(_ context.Context, _ *cli.Command) error {
					return exitcode.New(exitcode.PolicyDenied, "policy", errors.New("argv rejected"), "fix the input")
				},
			},
			{
				Name:  "upstream",
				Usage: "exit 3 (UpstreamFailed)",
				Description: `Returns exitcode.New(UpstreamFailed, ...). Process exits 3. Use this
when the wrapped tool ran and returned non-zero; the wrapper passes
the failure up but tags it as "from upstream" so the operator can
tell "the tool said no" apart from "the consumer said no".

Examples:

    exitcode-demo upstream ; echo "exit: $?"
    # error:
    #     kind: upstream_failed
    #     message: wrapped tool exited 7
    #     hint: check the tool
    # exit: 3

Retry semantics: read the underlying tool's stderr. The wrapper does
not retry for you.`,
				Action: func(_ context.Context, _ *cli.Command) error {
					return exitcode.New(exitcode.UpstreamFailed, "upstream_failed", errors.New("wrapped tool exited 7"), "check the tool")
				},
			},
			{
				Name:  "internal",
				Usage: "exit 4 (Internal)",
				Description: `Returns exitcode.New(Internal, ...). Process exits 4. Use this for
consumer-internal failures the user cannot fix from the call site:
config load failure, manifest miss, audit-write fail.

Examples:

    exitcode-demo internal ; echo "exit: $?"
    # error:
    #     kind: internal
    #     message: config load failed
    #     hint: report a bug
    # exit: 4

Treat exit 4 as host-broken-or-bug; escalate to operator, do not
retry on this host.`,
				Action: func(_ context.Context, _ *cli.Command) error {
					return exitcode.New(exitcode.Internal, "internal", errors.New("config load failed"), "report a bug")
				},
			},
		},
	}
}

// Policy is the tree for the metacharacter-gate demo.
func Policy() *cli.Command {
	return &cli.Command{
		Name:    "policy-demo",
		Usage:   "show shell-metacharacter argv rejection",
		Version: "v0.0.0",
		Description: `policy-demo exercises umbra's argv pre-validation gate. Every call
into a wrapped Action passes through policy.ValidateArg before execve.
The gate rejects any string containing one of these bytes:

    ` + "`" + `  $  ;  &  |  <  >  (  )  {  }  \  \n  \r  \t

Why this exists, even though umbra always builds an explicit argv
slice and never invokes /bin/sh: a non-trivial fraction of downstream
tools hand their last positional argument to a remote shell. Examples:

    ssh user@host '<remote-command>'
    kubectl exec pod -- sh -c '<command>'
    git config --global core.editor '<editor-cmd>'

If the agent driving umbra never sanitizes inputs and the wrapped
tool unsplats argv into a shell on the other side, a single semicolon
in an argument turns a benign verb into a chained injection. Rejecting
the metacharacters at the consumer boundary keeps that one-layer leak
from becoming an execution surprise two hops downstream.

Operating model for an agent calling these commands:

    - Any rejected input fails the verb deterministically with
      exitcode.PolicyDenied (2). The argv never reaches execve.
    - The error format is stable and parseable:
        policy: shell metacharacter rejected: arg <name> contains '<char>' at index <i>
    - On rejection, DO NOT retry with a quoted or escaped variant. The
      input is hostile by definition. Surface the rejection to the
      operator.
    - The gate is content-only. It does not check semantics. "rm -rf /"
      with no metacharacters passes the gate; whether the wrapped tool
      should run it is a separate verb-allowlist decision.

The two leaves below share an identical Action body. The only
difference is the input the operator types. That is the point: the
gate is the gate, not the leaf.`,
		Commands: []*cli.Command{
			{
				Name:      "safe",
				Usage:     "validates a single positional arg",
				ArgsUsage: "<value>",
				Description: `Validates a single positional argument against the ShellMeta byte set.
Prints the accepted value on success. Returns ErrShellMeta on
rejection (exit 2).

Examples:

    # accepted - no metacharacters
    policy-demo safe hello
    # accepted: [hello]

    # accepted - whitespace inside a single arg is fine, only \t \n \r reject
    policy-demo safe "hello world"
    # accepted: [hello world]

    # rejected - same code path as ` + "`unsafe`" + `, demonstrating the gate
    # is content-driven, not name-driven
    policy-demo safe 'hello; rm -rf /'
    # policy: shell metacharacter rejected: arg positional[0] contains ';' at index 5

The empty string passes the gate. Required-ness is a separate concern;
check len(args) before calling ValidateArgSlice if the verb needs at
least one value.`,
				Action: func(_ context.Context, c *cli.Command) error {
					vals := c.Args().Slice()
					if err := policy.ValidateArgSlice("positional", vals); err != nil {
						return err
					}
					fmt.Printf("accepted: %v\n", vals)
					return nil
				},
			},
			{
				Name:      "unsafe",
				Usage:     "demonstrate the rejection path",
				ArgsUsage: "<value-with-shell-metachar>",
				Description: `Identical Action body to ` + "`safe`" + `. Named "unsafe" only because the docs
expect the operator to feed it a metacharacter-bearing input. Useful
for an agent verifying the gate is wired before trusting the rest of
umbra's audit chain.

Examples:

    # the canonical rejection
    policy-demo unsafe 'foo; rm -rf /'
    # rejected: policy: shell metacharacter rejected: arg positional[0] contains ';' at index 3

    # backticks - command substitution attempt
    policy-demo unsafe '` + "`whoami`" + `'
    # rejected: policy: shell metacharacter rejected: arg positional[0] contains '` + "`" + `' at index 0

    # newline - multi-line injection attempt
    printf 'foo\nbar' | xargs -0 policy-demo unsafe
    # rejected: policy: shell metacharacter rejected: arg positional[0] contains '\n' at index 3

    # a clean input still passes here - the name is operator-facing,
    # the gate behavior is not
    policy-demo unsafe hello
    # accepted: [hello]

Agent behavior on rejection: parse the error string, report to the
operator, do not attempt to bypass. ShellMeta is a fixed list and is
not configurable per call. If a verb genuinely needs one of these
bytes in its input (rare; usually a sign the verb should be split
into pre-tokenized arguments instead), the right move is a code
change to that verb's ArgsFunc, not a runtime escape.`,
				Action: func(_ context.Context, c *cli.Command) error {
					vals := c.Args().Slice()
					if err := policy.ValidateArgSlice("positional", vals); err != nil {
						return err
					}
					fmt.Printf("accepted (unexpected): %v\n", vals)
					return nil
				},
			},
		},
	}
}
