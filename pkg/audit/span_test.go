package audit_test

import (
	"path/filepath"
	"testing"

	"github.com/coilyco/umbra/pkg/audit"
	"github.com/coilyco/umbra/pkg/exitcode"
)

// umbra#6817: the claim is that umbra separates refusal from failure where a
// log line does not. This is that separation, asserted one code at a time.
func TestOutcomeSeparatesRefusalFromFailure(t *testing.T) {
	cases := map[int]string{
		exitcode.Success:        audit.OutcomeOK,
		exitcode.PolicyDenied:   audit.OutcomeRefused,
		exitcode.UpstreamFailed: audit.OutcomeFailed,
		exitcode.Internal:       audit.OutcomeInternal,
		exitcode.UserError:      audit.OutcomeFailed,
		exitcode.Generic:        audit.OutcomeFailed,
	}
	for code, want := range cases {
		if got := audit.OutcomeFor(code); got != want {
			t.Errorf("OutcomeFor(%d) = %q, want %q", code, got, want)
		}
	}
}

// A refusal is a successful boundary. Marking its span an error would bury the
// signal in the same bucket as a broken upstream, which is the whole point.
func TestRefusalIsNotASpanError(t *testing.T) {
	refused := audit.Record{Verb: "aws.ssm.put-parameter", ExitCode: exitcode.PolicyDenied, Error: "policy_denied"}.SpanOf()
	if refused.Error {
		t.Error("a policy refusal must not be a span error")
	}
	if refused.Attributes[audit.AttrRefused] != true {
		t.Error("a refusal must be selectable by attribute, not by parsing an error string")
	}
	if refused.Attributes[audit.AttrKind] != "policy_denied" {
		t.Errorf("kind = %v, want the taxonomy's stable token", refused.Attributes[audit.AttrKind])
	}

	failed := audit.Record{Verb: "aws.ssm.get-parameter", ExitCode: exitcode.UpstreamFailed, Error: "boom"}.SpanOf()
	if !failed.Error {
		t.Error("an upstream failure must be a span error")
	}
	if failed.Attributes[audit.AttrRefused] != false {
		t.Error("a failure must not be counted as a refusal")
	}
}

func TestSpanCarriesTheVerbAndOmitsUnsetFields(t *testing.T) {
	s := audit.Record{Verb: "git.commit", Decision: "accept", ExitCode: 0}.SpanOf()
	if s.Name != "git.commit" {
		t.Errorf("name = %q, want the dotted verb path", s.Name)
	}
	for _, key := range []string{audit.AttrSessionID, audit.AttrRepoRoot, audit.AttrCacheStatus, audit.AttrPolicySkip, audit.AttrEgressHosts, audit.AttrKind} {
		if _, ok := s.Attributes[key]; ok {
			t.Errorf("unset field %s must be omitted, not emitted empty", key)
		}
	}
	// A nameless span is unqueryable, so an empty verb still gets a name.
	if (audit.Record{}).SpanOf().Name == "" {
		t.Error("an empty verb must still produce a named span")
	}
}

// The sink sees the record as written, after redaction, so it can never carry a
// value the durable JSONL would not.
func TestSinkReceivesTheRedactedRecord(t *testing.T) {
	var got []audit.Span
	w := audit.NewWriter(filepath.Join(t.TempDir(), "audit.jsonl"))
	w.Sinks = []audit.Sink{audit.SinkFunc(func(s audit.Span) { got = append(got, s) })}
	t.Cleanup(func() { _ = w.Close() })

	if err := w.Append(audit.Record{Verb: "git.push", Decision: "accept", ExitCode: 0}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("sink saw %d spans, want 1", len(got))
	}
	if got[0].Attributes[audit.AttrOutcome] != audit.OutcomeOK {
		t.Errorf("outcome = %v", got[0].Attributes[audit.AttrOutcome])
	}
}

// Telemetry must never fail an audited invocation, and a Writer with no sink
// must behave exactly as before.
func TestAppendSucceedsWithNoSinks(t *testing.T) {
	w := audit.NewWriter(filepath.Join(t.TempDir(), "audit.jsonl"))
	t.Cleanup(func() { _ = w.Close() })
	if err := w.Append(audit.Record{Verb: "git.status", Decision: "accept"}); err != nil {
		t.Fatalf("Append with no sinks: %v", err)
	}
}
