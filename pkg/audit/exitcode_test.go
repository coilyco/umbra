package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/coilyco/umbra/pkg/audit"
	"github.com/coilyco/umbra/pkg/exitcode"
)

// wrapOnce runs one recorded invocation returning err and reads the row back.
func wrapOnce(t *testing.T, err error) audit.Record {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := audit.NewWriter(path)
	if perr := w.Preflight(); perr != nil {
		t.Fatalf("preflight: %v", perr)
	}
	_ = w.Wrap(context.Background(), audit.Record{Verb: "demo.verb"}, func() error { return err })
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read audit: %v", rerr)
	}
	var rec audit.Record
	if jerr := json.Unmarshal(b, &rec); jerr != nil {
		t.Fatalf("decode row: %v (%s)", jerr, b)
	}
	return rec
}

func TestWrap_RecordsTheDeclaredExitCode(t *testing.T) {
	// A flat 1 for every failure is what made the log unable to separate a
	// guard refusal from a bad flag.
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantDec  string
	}{
		{"success", nil, exitcode.Success, audit.DecisionAccept},
		{"uncoded failure stays generic", errors.New("boom"), exitcode.Generic, audit.DecisionAccept},
		{
			name:     "a policy refusal is recorded as a rejection",
			err:      exitcode.New(exitcode.PolicyDenied, "policy_denied", errors.New("outside the allowed scope"), ""),
			wantCode: exitcode.PolicyDenied,
			wantDec:  audit.DecisionReject,
		},
		{
			// Distinct from a refusal: the caller was wrong, the guard was not.
			name:     "a user error is not a rejection",
			err:      exitcode.New(exitcode.UserError, "user_error", errors.New("missing --path"), ""),
			wantCode: exitcode.UserError,
			wantDec:  audit.DecisionAccept,
		},
		{
			name:     "an upstream failure keeps its own code",
			err:      exitcode.New(exitcode.UpstreamFailed, "upstream_failed", errors.New("502"), ""),
			wantCode: exitcode.UpstreamFailed,
			wantDec:  audit.DecisionAccept,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := wrapOnce(t, c.err)
			if rec.ExitCode != c.wantCode {
				t.Errorf("ExitCode = %d, want %d", rec.ExitCode, c.wantCode)
			}
			if rec.Decision != c.wantDec {
				t.Errorf("Decision = %q, want %q", rec.Decision, c.wantDec)
			}
		})
	}
}
