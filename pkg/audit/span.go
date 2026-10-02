// Span projection of an audit record. umbra ships no OTel SDK, the way it ships
// no store SDK: see docs/audit-spans.md.
package audit

import (
	"strconv"

	"github.com/coilyco/umbra/pkg/exitcode"
)

// Span attribute keys, namespaced so a collector can select umbra's rows
// without matching on a verb name. Stable: a consumer's dashboard queries them.
const (
	AttrVerb        = "umbra.verb"
	AttrDecision    = "umbra.decision"
	AttrOutcome     = "umbra.outcome"
	AttrExitCode    = "umbra.exit_code"
	AttrKind        = "umbra.kind"
	AttrRefused     = "umbra.refused"
	AttrSessionID   = "umbra.session_id"
	AttrRepoRoot    = "umbra.repo_root"
	AttrPolicySkip  = "umbra.policy_skipped"
	AttrCacheStatus = "umbra.cache"
	AttrEgressHosts = "umbra.egress_host_count"
)

// Outcome classes, the distinction the exit-code taxonomy already draws and a
// log line does not: a refusal is not a failure.
const (
	OutcomeOK       = "ok"
	OutcomeRefused  = "refused"
	OutcomeFailed   = "failed"
	OutcomeInternal = "internal"
)

// Span is one audit record projected onto a tracing span: a name, whether it
// represents an error, and flat attributes. No SDK type appears here.
type Span struct {
	Name       string
	StartUnix  int64
	DurationMS int64
	Error      bool
	Attributes map[string]any
}

// OutcomeFor classifies an exit code into the four span outcomes. This is the
// whole point of the projection: refused and failed are different populations.
func OutcomeFor(code int) string {
	switch code {
	case exitcode.Success:
		return OutcomeOK
	case exitcode.PolicyDenied:
		return OutcomeRefused
	case exitcode.Internal:
		return OutcomeInternal
	default:
		return OutcomeFailed
	}
}

// SpanOf projects a Record onto a Span. It reads the record as written, after
// redaction, so a sink can never see a value the JSONL would not carry.
func (r Record) SpanOf() Span {
	outcome := OutcomeFor(r.ExitCode)
	attrs := map[string]any{
		AttrVerb:     r.Verb,
		AttrDecision: r.Decision,
		AttrOutcome:  outcome,
		AttrExitCode: r.ExitCode,
		AttrRefused:  outcome == OutcomeRefused,
	}
	putIfSet(attrs, AttrSessionID, r.SessionID)
	putIfSet(attrs, AttrRepoRoot, r.RepoRoot)
	putIfSet(attrs, AttrCacheStatus, r.Cache)
	if r.PolicySkipped {
		attrs[AttrPolicySkip] = true
	}
	if n := len(r.Egress); n > 0 {
		attrs[AttrEgressHosts] = n
	}
	if r.Error != "" {
		attrs[AttrKind] = kindFromExit(r.ExitCode)
	}
	return Span{
		Name:       spanName(r.Verb),
		StartUnix:  r.Timestamp,
		DurationMS: r.DurationMS,
		// A refusal is a successful boundary, not a span error. Marking it an
		// error would bury it in the same bucket as a broken upstream.
		Error:      outcome == OutcomeFailed || outcome == OutcomeInternal,
		Attributes: attrs,
	}
}

// spanName keeps the dotted verb path, which is already the low-cardinality
// name a trace backend wants; an empty verb would otherwise mint a nameless span.
func spanName(verb string) string {
	if verb == "" {
		return "umbra.verb"
	}
	return verb
}

// kindFromExit renders the taxonomy's stable token for a code, so a query can
// select on the class rather than parsing an error string.
func kindFromExit(code int) string {
	switch code {
	case exitcode.PolicyDenied:
		return "policy_denied"
	case exitcode.UpstreamFailed:
		return "upstream_failed"
	case exitcode.Internal:
		return "internal"
	case exitcode.UserError:
		return "user_error"
	case exitcode.Success:
		return ""
	default:
		return "exit_" + strconv.Itoa(code)
	}
}

func putIfSet(attrs map[string]any, key, value string) {
	if value != "" {
		attrs[key] = value
	}
}

// Sink receives every appended record, for a consumer exporting spans. It runs
// after the JSONL write, so the durable log never depends on it.
type Sink interface {
	Emit(Span)
}

// SinkFunc adapts a func to Sink.
type SinkFunc func(Span)

// Emit calls f.
func (f SinkFunc) Emit(s Span) { f(s) }
