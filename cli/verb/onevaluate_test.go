package verb_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/pkg/audit"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/urfave/cli/v3"
)

func newWriterForTest(t *testing.T) (*audit.Writer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	w := audit.NewWriter(path)
	t.Cleanup(func() { _ = w.Close() })
	return w, path
}

func readLastRecord(t *testing.T, w *audit.Writer, path string) audit.Record {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		t.Fatalf("no audit lines, got: %q", string(b))
	}
	var rec audit.Record
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("unmarshal: %v\nline: %s", err, lines[len(lines)-1])
	}
	return rec
}

// TestOnEvaluate_NilLeavesBehaviorUnchanged confirms the field is
// strictly optional: a spec with no OnEvaluate matches pre-phase-4
func TestOnEvaluate_NilLeavesBehaviorUnchanged(t *testing.T) {
	w, path := newWriterForTest(t)
	called := false
	wrapped := verb.Wrap(verb.Spec{
		Name: "test.noop",
		Action: func(_ context.Context, _ *cli.Command) error {
			called = true
			return nil
		},
	}, w)
	cmd := &cli.Command{Name: "test", Action: wrapped}
	if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !called {
		t.Fatal("Action did not run")
	}
	rec := readLastRecord(t, w, path)
	if rec.ProfileDecision != nil {
		t.Errorf("expected nil ProfileDecision when OnEvaluate is nil, got %+v", rec.ProfileDecision)
	}
}

func TestOnEvaluate_AllowAttachesDecisionAndRunsAction(t *testing.T) {
	w, path := newWriterForTest(t)
	called := false
	pd := &audit.ProfileDecision{
		Allowed: true,
		Profile: "mac-tower",
		Source:  "override",
		Coordinate: audit.Coordinate{
			DataSecurity: "medium", BlastRadius: "high",
			NetworkEgress: "open", FilesystemReach: "unrestricted",
		},
		Reason: "axis not yet enforced",
	}
	wrapped := verb.Wrap(verb.Spec{
		Name: "test.allow",
		Action: func(_ context.Context, _ *cli.Command) error {
			called = true
			return nil
		},
		OnEvaluate: func(_ context.Context, _ *cli.Command) (*audit.ProfileDecision, error) {
			return pd, nil
		},
	}, w)
	cmd := &cli.Command{Name: "test", Action: wrapped}
	if err := cmd.Run(context.Background(), []string{"test"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !called {
		t.Fatal("Action did not run on allow")
	}
	rec := readLastRecord(t, w, path)
	if rec.ProfileDecision == nil {
		t.Fatalf("expected ProfileDecision attached, got nil")
	}
	if rec.ProfileDecision.Profile != "mac-tower" {
		t.Errorf("Profile = %q, want mac-tower", rec.ProfileDecision.Profile)
	}
	if !rec.ProfileDecision.Allowed {
		t.Errorf("Allowed = false, want true")
	}
}

func TestOnEvaluate_DenyShortCircuitsAndExitsPolicyDenied(t *testing.T) {
	w, path := newWriterForTest(t)
	called := false
	pd := &audit.ProfileDecision{
		Allowed: false,
		Profile: "headless",
		Source:  "override",
		Reason:  "data_security=max forbids this verb",
	}
	wrapped := verb.Wrap(verb.Spec{
		Name: "test.deny",
		Action: func(_ context.Context, _ *cli.Command) error {
			called = true
			return nil
		},
		OnEvaluate: func(_ context.Context, _ *cli.Command) (*audit.ProfileDecision, error) {
			return pd, errors.New("data_security=max forbids this verb")
		},
	}, w)
	cmd := &cli.Command{
		Name:           "test",
		Action:         wrapped,
		ExitErrHandler: func(_ context.Context, _ *cli.Command, _ error) {},
	}
	err := cmd.Run(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("expected deny error, got nil")
	}
	if called {
		t.Error("Action ran despite deny")
	}
	c := exitcode.From(err)
	if c == nil || c.Code() != exitcode.PolicyDenied {
		t.Errorf("exit code = %v, want PolicyDenied", c)
	}
	// The lockdown-axis deny must carry its own Reasoner why-line.
	var rsn exitcode.Reasoner
	if !errors.As(err, &rsn) || rsn.Reason() == "" {
		t.Errorf("lockdown policy_denied carried no Reason(); want the lockdown-axis why-line")
	}
	rec := readLastRecord(t, w, path)
	if rec.Decision != audit.DecisionReject {
		t.Errorf("decision = %q, want reject", rec.Decision)
	}
	if rec.ProfileDecision == nil || rec.ProfileDecision.Allowed {
		t.Errorf("expected attached deny decision, got %+v", rec.ProfileDecision)
	}
}

// TestOnEvaluate_EvaluatorFailedHintIsConsumerAgnostic: an internal evaluator
// error surfaces a hint that names no consumer filename.
func TestOnEvaluate_EvaluatorFailedHintIsConsumerAgnostic(t *testing.T) {
	w, _ := newWriterForTest(t)
	wrapped := verb.Wrap(verb.Spec{
		Name:   "test.evalfail",
		Action: func(_ context.Context, _ *cli.Command) error { return nil },
		OnEvaluate: func(_ context.Context, _ *cli.Command) (*audit.ProfileDecision, error) {
			return nil, errors.New("boom")
		},
	}, w)
	cmd := &cli.Command{
		Name:           "test",
		Action:         wrapped,
		ExitErrHandler: func(_ context.Context, _ *cli.Command, _ error) {},
	}
	err := cmd.Run(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("expected evaluator_failed error, got nil")
	}
	var coded *exitcode.CodedError
	if !errors.As(err, &coded) {
		t.Fatalf("error is not a *exitcode.CodedError: %v", err)
	}
	if coded.Kind() != "evaluator_failed" {
		t.Errorf("kind = %q, want evaluator_failed", coded.Kind())
	}
	if strings.Contains(coded.HintText(), "coily") {
		t.Errorf("hint leaks a consumer path: %q", coded.HintText())
	}
	if !strings.Contains(coded.HintText(), "lockdown profile config") {
		t.Errorf("hint = %q, want the consumer-agnostic config role", coded.HintText())
	}
}

// TestOnEvaluate_EvaluatorFailedHintNamesConfiguredFile confirms a consumer
// that sets EvaluatorConfigHint gets its own filename surfaced.
func TestOnEvaluate_EvaluatorFailedHintNamesConfiguredFile(t *testing.T) {
	w, _ := newWriterForTest(t)
	wrapped := verb.Wrap(verb.Spec{
		Name:                "test.evalfail",
		EvaluatorConfigHint: ".myapp/myapp.yaml",
		Action:              func(_ context.Context, _ *cli.Command) error { return nil },
		OnEvaluate: func(_ context.Context, _ *cli.Command) (*audit.ProfileDecision, error) {
			return nil, errors.New("boom")
		},
	}, w)
	cmd := &cli.Command{
		Name:           "test",
		Action:         wrapped,
		ExitErrHandler: func(_ context.Context, _ *cli.Command, _ error) {},
	}
	err := cmd.Run(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("expected evaluator_failed error, got nil")
	}
	var coded *exitcode.CodedError
	if !errors.As(err, &coded) {
		t.Fatalf("error is not a *exitcode.CodedError: %v", err)
	}
	if !strings.Contains(coded.HintText(), ".myapp/myapp.yaml") {
		t.Errorf("hint = %q, want the configured filename", coded.HintText())
	}
}

func TestProfileDecision_RoundTripsJSON(t *testing.T) {
	pd := audit.ProfileDecision{
		Allowed: true,
		Profile: "mobile",
		Source:  "override",
		Coordinate: audit.Coordinate{
			DataSecurity: "high", BlastRadius: "medium",
			NetworkEgress: "allowlisted", FilesystemReach: "repo-plus-home",
		},
		Reason: "ok",
	}
	b, err := json.Marshal(pd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got audit.ProfileDecision
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != pd {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, pd)
	}
}
