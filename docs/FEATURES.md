# umbra features

Inventory of umbra today, grouped by **guarded surface** over a shared `pkg/`. See [architecture.md](architecture.md); each primitive ships a walkthrough in `guides/`. Dev verbs run through the `Makefile`; release is automated and Forgejo-canonical, with commit-scoped draft tags on `main` ([release-pipeline.md](release-pipeline.md)).

## CLI exec surface (`cli/`)

- **execverb** - Exec-dialect KDL verbs, complex actions, and inspect lists. A guardfile may `withhold` a verb as a stated refusal rather than a silent absence, `pin` a flag to one value umbra supplies and a caller cannot override, and declare `resolve-flag` so umbra resolves that flag's value through `pkg/valuesource` and spills it to a file rather than forwarding an argv token. See [execverb.md](execverb.md) and [occlusion primitives](execverb-occlusion.md).
- **execverb default-allow** - A wrap declaring `default-allow` inverts the dialect's closed default for that wrap alone: an unnamed verb is forwarded to the wrapped binary and the guardfile names only what it refuses or constrains. The declaration requires a `reason`, the wrap-level guards and env injections still bind on a forwarded call, and the shapes where it would have two answers (a `can run *` funnel, an `allow` list, or naming nothing at all) fail closed at parse. Paired with `replace` it is what lets a replacement name only its boundary rather than inventory the tool. See [default-allow](execverb-default-allow.md).
- **execverb replacements** - A wrap declaring `replace` builds a binary installed under the wrapped tool's own name rather than as a verb under a driver's tree, so the guardfile's grants are the whole tool a caller sees. Real-binary resolution excludes the running executable and the child's PATH loses the shim directory, because a wrapper that resolves its own name finds itself. `umbra install` places it and `umbra doctor` reports what the installation occludes, including the finding that never passes. See [occluded replacement binaries](execverb-replacement.md).
- **verb** - Middleware wrapping every `*cli.Command.Action` in the validate -> execute -> audit pipeline, with audited argv and env injection.

## HTTP request surface (`http/`)

- **specverb / guardfile** - Spec-driven verbs: [resolution](specverb-resolution.md), [policy](specverb-policy.md), [requests](specverb-request.md), [actions](specverb-actions.md), [describe](specverb-describe.md), [fetch](specverb-fetch.md), [descriptors](specverb-descriptors.md) for a consumer that mounts operations onto something other than a cli tree.
- **mcpverb** - MCP-shaped verbs: one `can call` grant per guarded leaf against an upstream MCP server, flags typed from the committed tool lock, deny by absence. `ServedTools` projects the same grants back into tool definitions for a consumer that serves them, and a grant's `widget` block declares what that tool's MCP Apps view may call back. See [mcpverb.md](mcpverb.md).
- **mcpverb upstream guardfiles** - The other shape a `.mcp.kdl` file takes: `mcp-upstream` states where a proxied MCP server is, what credential reaches it, and which of its tools may be called, with no command path and no tool lock. `Classify` tells the two shapes apart before either parser runs. See [mcpverb-upstream.md](mcpverb-upstream.md).
- **umbra / codegen** - The no-code driver: discovery, locks, generation, over three transports (spec, exec, mcp). See [umbra-cli.md](umbra-cli.md) and [materialization](umbra-materialization.md).
- **opcore** - The frozen inline grammar: typed query, body projection, GraphQL and SQL grants, JMESPath postconditions, MCP proxy grants, and a named client. See [opcore-inline.md](opcore-inline.md) and [opcore-body.md](opcore-body.md).
- **respfmt** - JSON renderer with optional JMESPath projection and five output formats, mirroring the aws CLI `--query` / `--output` surface.
- **openapigen** - `umbra openapi` renders the granted subset of an upstream as an OpenAPI 3.1 document, so a KDL-defined policy is consumable by spec-driven tooling. Pinned body values emit as `const`, withheld verbs are absent, and a leaf reaching no URL is skipped and named. See [openapigen.md](openapigen.md).

## Shared core (`pkg/`)

- **audit** - Append-only JSONL invocation log with rotation and optional typed CI attribution, which it preserves but does not establish trust in. Records project onto tracing spans through `Record.SpanOf()` and a `Sink`, carrying the exit-code taxonomy so a refusal is distinguishable from a failure. `pkg/audit/otelsink` emits them as OpenTelemetry spans and can wire an OTLP exporter. See [audit spans](audit-spans.md).
- **policy** - Argv validation rejecting shell metacharacters before `execve`. On the HTTP surfaces the gate is location-scoped to path values, the only inputs `FillPath` substitutes unescaped, and a wrap opts a named path param out with `allow-metacharacters`. See [the gate section](specverb-request.md#the-shell-metachar-gate-is-location-aware).
- **scope** / **exitcode** - Resolve cwd to its git toplevel for each audit row's RepoRoot, and a public exit-code taxonomy for orchestrators. A generated binary exits with the code its error declares (2 for a policy refusal, 5 for a user error), and the audit row records the same code plus a `reject` decision.
- **valuesource** / **tokenmint** - Shared `value <provider>` resolution with
  fallback chains, plus OAuth `client_credentials` tokens minted rather than
  read. See [value providers](value-providers.md).
- **config** / **stepflow** / **flock** / **skillgen** - Cache and audit-path derivation rooted at the consumer's app dir, a transport-agnostic ordered sequence engine, an advisory build lock, and the skill projection the driver emits.
- **mcpclient** - The Model Context Protocol client the mcp dialect speaks: one declared upstream over stdio or Streamable HTTP, the calls the dialect needs, and an optional progress sink. Policy-free, so it sits in the core.
- **mcpapps** - The MCP Apps host bridge: the frames a rendered widget sends back, answered under the guardfile's `widget` block rather than forwarded. Tool calls, resource reads, link opens, and downloads each take their own grant, and progress rides back under the view's own token. Transport-free and policy-free, so a consumer supplies the presenter and `http/mcpverb` supplies the policy. See [mcpapps.md](mcpapps.md).

## Two front doors

Every package above is reached through **umbra** (the driver and the binaries it generates) or through **beaver** (`mcp-beaver`, which imports `guardfile`, `opcore`, `specverb`, `tokenmint`, and `valuesource`). A package no front door reaches does not belong here.

ward was a third door and is deprecated. What only ward needed - `cli/{gittree,passthrough,repocfg,shell}`, `http/egress`, and `pkg/{attribution,broker,credseed,issueref,ownertrust,provenance,scan,version}` - was removed rather than kept for a consumer that is going away.

## See also

- [README.md](../README.md) - human-facing intro.
- [AGENTS.md](../AGENTS.md) - agent-facing operating rules.

Cross-reference convention from the shared repo-pointer rule in the agentic-os docs.
