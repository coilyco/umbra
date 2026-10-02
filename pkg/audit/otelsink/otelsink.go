// Package otelsink emits umbra audit records as OpenTelemetry spans. The
// projection lives in pkg/audit; this binds it to a tracer.
package otelsink

import (
	"context"
	"time"

	"github.com/coilyco/umbra/pkg/audit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope every umbra span carries, so a
// collector can select umbra's rows by scope rather than by span name.
const ScopeName = "github.com/coilyco/umbra"

// Sink emits one span per appended audit record.
type Sink struct {
	tracer trace.Tracer
	// Now backstops a record with no timestamp; tests override it.
	Now func() time.Time
}

// New returns a Sink emitting through tracer. A nil tracer makes Emit a no-op,
// so a consumer that has not wired a provider is not a crash.
func New(tracer trace.Tracer) *Sink {
	return &Sink{tracer: tracer, Now: time.Now}
}

// Emit records one span. An audit record describes completed work, so the span
// opens at the record's start and closes at start+duration rather than now.
func (s *Sink) Emit(sp audit.Span) {
	if s == nil || s.tracer == nil {
		return
	}
	start := time.Unix(sp.StartUnix, 0)
	if sp.StartUnix == 0 {
		start = s.now()
	}
	_, span := s.tracer.Start(context.Background(), sp.Name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(start),
		trace.WithAttributes(attributesOf(sp)...),
	)
	// A refusal sets Error=false: it is a successful boundary, and the status
	// must not put it in the same bucket as a broken upstream.
	if sp.Error {
		span.SetStatus(codes.Error, kindOf(sp))
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End(trace.WithTimestamp(start.Add(time.Duration(sp.DurationMS) * time.Millisecond)))
}

func (s *Sink) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// kindOf renders the taxonomy token for a span status description, "" absent.
func kindOf(sp audit.Span) string {
	if v, ok := sp.Attributes[audit.AttrKind].(string); ok {
		return v
	}
	return ""
}

// attributesOf lowers the projection's flat map onto typed OTel attributes.
// An unexpected type renders as a string rather than being dropped.
func attributesOf(sp audit.Span) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(sp.Attributes))
	for k, v := range sp.Attributes {
		switch t := v.(type) {
		case string:
			out = append(out, attribute.String(k, t))
		case bool:
			out = append(out, attribute.Bool(k, t))
		case int:
			out = append(out, attribute.Int(k, t))
		default:
			out = append(out, attribute.String(k, stringify(t)))
		}
	}
	return out
}
