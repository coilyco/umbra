package exitcode_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/coilyco/umbra/pkg/exitcode"
)

func TestCodedError_RoundTrip(t *testing.T) {
	root := errors.New("root cause")
	c := exitcode.New(exitcode.PolicyDenied, "policy_denied", root, "hint text")

	if c.Code() != exitcode.PolicyDenied {
		t.Errorf("Code() = %d, want %d", c.Code(), exitcode.PolicyDenied)
	}
	if c.Kind() != "policy_denied" {
		t.Errorf("Kind() = %q, want policy_denied", c.Kind())
	}
	if !errors.Is(c, root) {
		t.Errorf("errors.Is(c, root) = false, want true (Unwrap broken)")
	}
}

func TestFrom_FindsCodedDeepInChain(t *testing.T) {
	c := exitcode.New(exitcode.UpstreamFailed, "upstream_failed", errors.New("inner"), "")
	wrapped := fmt.Errorf("outer: %w", c)
	got := exitcode.From(wrapped)
	if got == nil {
		t.Fatal("From returned nil; want the embedded coded error")
	}
	if got.Code() != exitcode.UpstreamFailed {
		t.Errorf("got.Code() = %d, want %d", got.Code(), exitcode.UpstreamFailed)
	}
}

// TestCodedError_WithReason pins the Reason() contract: Reason() is
// the optional second-line companion to HintText() and is empty by
func TestCodedError_WithReason(t *testing.T) {
	c := exitcode.New(exitcode.PolicyDenied, "policy_denied", errors.New("x"), "do this")
	if c.Reason() != "" {
		t.Errorf("Reason() before WithReason = %q, want empty", c.Reason())
	}
	c.WithReason("because the gate exists for that reason")
	if c.Reason() != "because the gate exists for that reason" {
		t.Errorf("Reason() after WithReason = %q", c.Reason())
	}

	// Reasoner interface satisfied by *CodedError; consumers can errors.As.
	var r exitcode.Reasoner
	if !errors.As(error(c), &r) {
		t.Fatal("errors.As against Reasoner failed; CodedError must satisfy the interface")
	}
	if r.Reason() != "because the gate exists for that reason" {
		t.Errorf("Reasoner.Reason() = %q, want the attached value", r.Reason())
	}
}

// TestCodedError_WithReason_NilReceiver guards the chain shape
// `exitcode.New(...).WithReason(...)` against accidental nil chains.
func TestCodedError_WithReason_NilReceiver(t *testing.T) {
	var c *exitcode.CodedError
	if got := c.WithReason("x"); got != nil {
		t.Errorf("nil receiver should stay nil, got %+v", got)
	}
}

func TestFrom_NilOnPlainError(t *testing.T) {
	if got := exitcode.From(errors.New("plain")); got != nil {
		t.Errorf("From(plain) = %v, want nil", got)
	}
}
