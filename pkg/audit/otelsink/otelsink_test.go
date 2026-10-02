package otelsink_test

import (
	"context"
	"testing"
	"time"

	"github.com/coilyco/umbra/pkg/audit"
	"github.com/coilyco/umbra/pkg/audit/otelsink"
	"github.com/coilyco/umbra/pkg/exitcode"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func recorded(t *testing.T, rec audit.Record) tracetest.SpanStub {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	otelsink.New(tp.Tracer("test")).Emit(rec.SpanOf())
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	return spans[0]
}

func attrOf(s tracetest.SpanStub, key string) attribute.Value {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

// umbra#6817: the whole claim is that a refusal is distinguishable from a
// failure. Assert it on the emitted span, not just the projection.
func TestRefusalIsOkStatusWithRefusedTrue(t *testing.T) {
	s := recorded(t, audit.Record{
		Verb: "aws.ssm.put-parameter", ExitCode: exitcode.PolicyDenied,
		Error: "policy_denied", Timestamp: 1756800000, DurationMS: 12,
	})
	if s.Status.Code != codes.Ok {
		t.Errorf("status = %v, want Ok: a refusal is a successful boundary", s.Status.Code)
	}
	if attrOf(s, audit.AttrRefused).AsBool() != true {
		t.Error("umbra.refused must be true on a refusal")
	}
	if got := attrOf(s, audit.AttrKind).AsString(); got != "policy_denied" {
		t.Errorf("umbra.kind = %q", got)
	}
}

func TestUpstreamFailureIsErrorStatus(t *testing.T) {
	s := recorded(t, audit.Record{
		Verb: "aws.ssm.get-parameter", ExitCode: exitcode.UpstreamFailed,
		Error: "boom", Timestamp: 1756800000,
	})
	if s.Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", s.Status.Code)
	}
	if s.Status.Description != "upstream_failed" {
		t.Errorf("description = %q, want the taxonomy token", s.Status.Description)
	}
	if attrOf(s, audit.AttrRefused).AsBool() != false {
		t.Error("a failure must not be counted as a refusal")
	}
}

// An audit record describes completed work, so the span must span the recorded
// window rather than a zero-width instant at export time.
func TestSpanCoversTheRecordedWindow(t *testing.T) {
	s := recorded(t, audit.Record{Verb: "git.push", Timestamp: 1756800000, DurationMS: 250})
	if got := s.EndTime.Sub(s.StartTime); got != 250*time.Millisecond {
		t.Errorf("duration = %v, want 250ms", got)
	}
	if s.StartTime.Unix() != 1756800000 {
		t.Errorf("start = %v, want the record's own timestamp", s.StartTime)
	}
	if s.Name != "git.push" {
		t.Errorf("name = %q", s.Name)
	}
}

// A consumer that has not wired a provider must not crash the invocation it
// was auditing.
func TestNilTracerIsANoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("an unwired sink must not panic the invocation it audits: %v", r)
		}
	}()
	otelsink.New(nil).Emit(audit.Record{Verb: "git.status"}.SpanOf())
	var nilSink *otelsink.Sink
	nilSink.Emit(audit.Record{Verb: "git.status"}.SpanOf())
}

func TestProviderRequiresAServiceName(t *testing.T) {
	if _, err := otelsink.NewProvider(context.Background(), otelsink.ProviderConfig{}); err == nil {
		t.Fatal("an unnamed service is unattributable and must fail closed")
	}
}
