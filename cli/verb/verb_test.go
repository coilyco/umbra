package verb_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/pkg/audit"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/policy"
	"github.com/urfave/cli/v3"
)

func newTestWriter(t *testing.T) *audit.Writer {
	t.Helper()
	w := &audit.Writer{
		Path: filepath.Join(t.TempDir(), "audit.jsonl"),
		Now:  func() time.Time { return time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC) },
	}
	// Close on cleanup so Windows can actually remove the TempDir. Lumberjack
	// keeps the fd open otherwise and the post-test RemoveAll fails.
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestWrap_RunsAction(t *testing.T) {
	called := false
	spec := verb.Spec{
		Name: "test.ro",
		Action: func(_ context.Context, _ *cli.Command) error {
			called = true
			return nil
		},
	}
	if err := runWrapped(t, spec, newTestWriter(t)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !called {
		t.Error("action was not called")
	}
}

func TestWrap_RejectsShellMetacharInArg(t *testing.T) {
	w := newTestWriter(t)
	spec := verb.Spec{
		Name: "test.ro",
		ArgsFunc: func(_ *cli.Command) (map[string]string, []string) {
			return map[string]string{"--thing": "foo;bar"}, nil
		},
		Action: func(_ context.Context, _ *cli.Command) error {
			t.Error("action should not be called when args fail policy")
			return nil
		},
	}
	err := runWrapped(t, spec, w)
	if !errors.Is(err, policy.ErrShellMeta) {
		t.Errorf("err = %v, want ErrShellMeta", err)
	}
	// The shell-meta gate must attach a Reasoner why-line so a consumer
	// envelope needs no fallback map.
	var rsn exitcode.Reasoner
	if !errors.As(err, &rsn) || rsn.Reason() == "" {
		t.Errorf("policy_denied carried no Reason(); want the shell-meta invariant why-line")
	}
	b, _ := os.ReadFile(w.Path)
	records, _ := audit.ReadAll(bytes.NewReader(b))
	if len(records) != 1 {
		t.Fatalf("got %d audit records on policy reject, want 1", len(records))
	}
	if records[0].Decision != audit.DecisionReject {
		t.Errorf("decision = %q, want %q", records[0].Decision, audit.DecisionReject)
	}
	if records[0].ExitCode != exitcode.PolicyDenied {
		t.Errorf("exit_code = %d, want %d, what the process exits with", records[0].ExitCode, exitcode.PolicyDenied)
	}
}

func TestWrap_RejectsShellMetacharInPositional(t *testing.T) {
	spec := verb.Spec{
		Name: "test.ro",
		ArgsFunc: func(_ *cli.Command) (map[string]string, []string) {
			return nil, []string{"ok", "bad;bar"}
		},
		Action: func(_ context.Context, _ *cli.Command) error {
			t.Error("action should not be called when positional fails policy")
			return nil
		},
	}
	err := runWrapped(t, spec, newTestWriter(t))
	if !errors.Is(err, policy.ErrShellMeta) {
		t.Errorf("err = %v, want ErrShellMeta", err)
	}
}

func TestWrap_WritesAuditRecord(t *testing.T) {
	w := newTestWriter(t)
	spec := verb.Spec{
		Name:   "test.ro",
		Action: func(_ context.Context, _ *cli.Command) error { return nil },
	}
	if err := runWrapped(t, spec, w); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(w.Path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if len(b) == 0 {
		t.Error("audit file is empty")
	}
	records, err := audit.ReadAll(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("parse audit: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Verb != "test.ro" {
		t.Errorf("verb = %q", records[0].Verb)
	}
	if records[0].Decision != audit.DecisionAccept {
		t.Errorf("decision = %q, want %q", records[0].Decision, audit.DecisionAccept)
	}
}

// TestWrap_RecordsCWDFields pins the audit CWD field: every audit
// row carries CWDSubprocess (always populated from os.Getwd at record
func TestWrap_RecordsCWDFields(t *testing.T) {
	w := newTestWriter(t)
	spec := verb.Spec{
		Name:             "test.cwd",
		ResolveInvokeCWD: func() string { return "/some/operator/cwd" },
		Action:           func(_ context.Context, _ *cli.Command) error { return nil },
	}
	if err := runWrapped(t, spec, w); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(w.Path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	records, err := audit.ReadAll(bytes.NewReader(b))
	if err != nil || len(records) != 1 {
		t.Fatalf("parse: %d records, err=%v", len(records), err)
	}
	if records[0].CWDSubprocess == "" {
		t.Error("CWDSubprocess is empty; should always be populated from os.Getwd")
	}
	if records[0].CWDAtInvocation != "/some/operator/cwd" {
		t.Errorf("CWDAtInvocation = %q, want /some/operator/cwd", records[0].CWDAtInvocation)
	}
}

// TestWrap_RecordsCWDFields_NoResolverLeavesInvocationEmpty confirms the
// nil-resolver default: CWDSubprocess still populated, CWDAtInvocation
func TestWrap_RecordsCWDFields_NoResolverLeavesInvocationEmpty(t *testing.T) {
	w := newTestWriter(t)
	spec := verb.Spec{
		Name:   "test.cwd-no-resolver",
		Action: func(_ context.Context, _ *cli.Command) error { return nil },
	}
	if err := runWrapped(t, spec, w); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(w.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	records, err := audit.ReadAll(bytes.NewReader(b))
	if err != nil || len(records) != 1 {
		t.Fatalf("parse: %d records, err=%v", len(records), err)
	}
	if records[0].CWDSubprocess == "" {
		t.Error("CWDSubprocess empty without resolver")
	}
	if records[0].CWDAtInvocation != "" {
		t.Errorf("CWDAtInvocation = %q, want empty when ResolveInvokeCWD is nil", records[0].CWDAtInvocation)
	}
}

func TestWrap_NilWriterStillRunsAction(t *testing.T) {
	called := false
	spec := verb.Spec{
		Name: "test.ro",
		Action: func(_ context.Context, _ *cli.Command) error {
			called = true
			return nil
		},
	}
	if err := runWrapped(t, spec, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !called {
		t.Error("action was not called")
	}
}

func TestWrap_RecordsFailureInAudit(t *testing.T) {
	w := newTestWriter(t)
	spec := verb.Spec{
		Name: "test.ro",
		Action: func(_ context.Context, _ *cli.Command) error {
			return errors.New("boom")
		},
	}
	err := runWrapped(t, spec, w)
	if err == nil {
		t.Fatal("expected error")
	}
	b, _ := os.ReadFile(w.Path)
	records, _ := audit.ReadAll(bytes.NewReader(b))
	if len(records) != 1 || records[0].ExitCode != 1 {
		t.Errorf("records = %+v, want one record with exit_code=1", records)
	}
}

func TestWrap_OnCompleteMutatesRecord(t *testing.T) {
	w := newTestWriter(t)
	spec := verb.Spec{
		Name:   "test.ro",
		Action: func(_ context.Context, _ *cli.Command) error { return nil },
		OnComplete: func(r *audit.Record) {
			r.Egress = []audit.EgressRow{
				{Host: "formulae.brew.sh", Decision: audit.EgressAllow, BytesUp: 100, BytesDown: 200, DurationMS: 5},
			}
		},
	}
	if err := runWrapped(t, spec, w); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, _ := os.ReadFile(w.Path)
	records, _ := audit.ReadAll(bytes.NewReader(b))
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if len(records[0].Egress) != 1 || records[0].Egress[0].Host != "formulae.brew.sh" {
		t.Errorf("egress = %+v, want one row for formulae.brew.sh", records[0].Egress)
	}
}

// TestWrap_IDOverridePinsAuditRowID proves Spec.IDOverride wins over the
// audit writer's auto-generated UUID v7. Used by the consumer's ssh passthrough
func TestWrap_IDOverridePinsAuditRowID(t *testing.T) {
	w := newTestWriter(t)
	const pinned = "01234567-89ab-7def-0123-456789abcdef"
	spec := verb.Spec{
		Name:       "test.ro",
		IDOverride: pinned,
		Action:     func(_ context.Context, _ *cli.Command) error { return nil },
	}
	if err := runWrapped(t, spec, w); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, _ := os.ReadFile(w.Path)
	records, _ := audit.ReadAll(bytes.NewReader(b))
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].ID != pinned {
		t.Errorf("ID = %q, want %q", records[0].ID, pinned)
	}
}

// runWrapped invokes the wrapped action in a way that mimics urfave/cli's
// real invocation shape. We pass an empty *cli.Command because Spec.ArgsFunc
func runWrapped(t *testing.T, spec verb.Spec, w *audit.Writer) error {
	t.Helper()
	action := verb.Wrap(spec, w)
	return action(context.Background(), &cli.Command{Name: spec.Name})
}
