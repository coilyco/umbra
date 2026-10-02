// Package audit writes one JSONL record per CLI invocation to an
// append-only log outside the working tree. Per SECURITY.md, the
package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/coilyco/umbra/pkg/exitcode"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Record is one line in the audit log. Timestamp is unix seconds (int64),
// JSON-encoded as a number. Easier to sort and diff than RFC3339 strings.
type Record struct {
	// ID is a UUID v7 (time-ordered) populated on Append if unset. The
	// stable identifier for an audit row, used to join cross-host records.
	ID        string   `json:"id,omitempty"`
	Timestamp int64    `json:"ts"`
	Version   string   `json:"version,omitempty"`
	Decision  string   `json:"decision"`
	Verb      string   `json:"verb"`
	Argv      []string `json:"argv"`
	ExitCode  int      `json:"exit_code"`
	Error     string   `json:"error,omitempty"`
	// StderrTail is a bounded last-N-bytes capture of the wrapped tool's
	// stderr, populated by pass-through verbs on non-zero exit so the audit
	StderrTail string `json:"stderr_tail,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	// RepoRoot is git rev-parse --show-toplevel of cwd at invocation time,
	// or empty if cwd was not inside a git repo. Forensic only: tells the
	RepoRoot string `json:"repo_root,omitempty"`
	// CWDSubprocess is os.Getwd() captured by buildBaseRecord at the
	// moment the subprocess saw the world. Differs from CWDAtInvocation
	CWDSubprocess string `json:"cwd_subprocess,omitempty"`
	// CWDAtInvocation is the consumer-resolved operator cwd, populated
	// from verb.Spec.ResolveInvokeCWD when set. Empty when the consumer
	CWDAtInvocation string `json:"cwd_at_invocation,omitempty"`
	// SessionID joins this row to the Claude Code (or other agent harness)
	// session that produced the invocation. Resolution order at write time:
	SessionID string `json:"session_id,omitempty"`
	// AuditOverride is set true when a repo verb ran with
	// --audit-override-dirty: the clean+synced gate refused but the
	AuditOverride bool `json:"audit_override,omitempty"`
	// WorkingTreeStatus is the truncated `git status --porcelain` output
	// captured when a repo verb ran with --audit-override-dirty. Empty for
	WorkingTreeStatus string `json:"working_tree_status,omitempty"`
	// CI carries consumer-validated continuous-integration attribution.
	// The audit package preserves this evidence but does not establish trust.
	CI *CIContext `json:"ci,omitempty"`
	// Egress carries one row per host contacted by the wrapped subprocess
	// when the verb runs through the per-invocation HTTP CONNECT proxy
	Egress []EgressRow `json:"egress,omitempty"`
	// ProfileDecision captures the per-session lockdown-profile evaluation
	// for this verb, when a consumer has wired verb.Spec.OnEvaluate. Absent
	ProfileDecision *ProfileDecision `json:"profile_decision,omitempty"`
	// PolicySkipped is true when the shell-metacharacter validator was
	// bypassed for this invocation. Set by consumers whose verb wiring
	PolicySkipped bool `json:"policy_skipped,omitempty"`
	// Cache is the TTL-cache disposition of a cache-eligible action, e.g. "hit"
	// when a collect served from cache. Empty when no cache was consulted.
	Cache string `json:"cache,omitempty"`
}

// CIContext is provider-neutral CI attribution supplied by consumers.
// Audit preserves it but does not establish trust or validate identifiers.
type CIContext struct {
	Provider    string `json:"provider"`
	ServerURL   string `json:"server_url,omitempty"`
	Repository  string `json:"repository"`
	EventName   string `json:"event_name"`
	EventRef    string `json:"event_ref"`
	PullRequest string `json:"pull_request,omitempty"`
	BaseRef     string `json:"base_ref,omitempty"`
	HeadRef     string `json:"head_ref,omitempty"`
	HeadSHA     string `json:"head_sha"`
	Workflow    string `json:"workflow,omitempty"`
	Job         string `json:"job,omitempty"`
	Actor       string `json:"actor,omitempty"`
	RunID       string `json:"run_id"`
	RunNumber   string `json:"run_number,omitempty"`
	RunAttempt  string `json:"run_attempt,omitempty"`
}

// ProfileDecision is the structured outcome of a per-session
// lockdown-profile evaluation. Allowed=false plus a non-nil verb.Spec
type ProfileDecision struct {
	Allowed    bool       `json:"allowed"`
	Profile    string     `json:"profile,omitempty"`
	Source     string     `json:"source,omitempty"`
	Coordinate Coordinate `json:"coordinate"`
	Reason     string     `json:"reason,omitempty"`
}

// Coordinate mirrors umbra/profile.Coordinate as a JSON-stable
// snapshot. Duplicated here so audit.Record (the wire format) does not
type Coordinate struct {
	DataSecurity    string `json:"data_security,omitempty"`
	BlastRadius     string `json:"blast_radius,omitempty"`
	NetworkEgress   string `json:"network_egress,omitempty"`
	FilesystemReach string `json:"filesystem_reach,omitempty"`
}

// EgressRow is one (parent-invocation, host) pair from the egress proxy.
// Decision is "allow" or "deny"; deny rows are produced when the host fails
type EgressRow struct {
	Host       string `json:"host"`
	Decision   string `json:"decision"`
	BytesUp    int64  `json:"bytes_up"`
	BytesDown  int64  `json:"bytes_down"`
	DurationMS int64  `json:"duration_ms"`
}

// Egress decision values.
const (
	EgressAllow = "allow"
	EgressDeny  = "deny"
)

// MaxStderrTailBytes caps Record.StderrTail so the audit row stays small
// even when a wrapped tool spews. 2 KiB is enough to carry the last few
const MaxStderrTailBytes = 2048

// newUUIDv7 returns a UUID v7 string (time-ordered) using crypto/rand.
// 48-bit unix-millis prefix, version=7, variant=10, rest random.
func newUUIDv7(now time.Time) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("audit: rand: %w", err)
	}
	ms := uint64(now.UnixMilli()) //nolint:gosec // UnixMilli fits in 48 bits for the next ~8000 years
	b[0] = byte(ms >> 40 & 0xff)  //nolint:gosec // mask makes overflow inert
	b[1] = byte(ms >> 32 & 0xff)  //nolint:gosec
	b[2] = byte(ms >> 24 & 0xff)  //nolint:gosec
	b[3] = byte(ms >> 16 & 0xff)  //nolint:gosec
	b[4] = byte(ms >> 8 & 0xff)   //nolint:gosec
	b[5] = byte(ms & 0xff)        //nolint:gosec
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Decision values for Record.Decision.
const (
	DecisionAccept = "accept"
	DecisionReject = "reject"
)

// Writer appends records to a JSONL file. The zero value is unusable. Use
// NewWriter or set Path explicitly.
type Writer struct {
	// Path is the JSONL file. Must be set.
	Path string
	// Now is used for timestamps. Tests override. Defaults to time.Now.
	Now func() time.Time
	// MaxSizeMB is the rotation trigger. Zero uses lumberjack's default (100).
	MaxSizeMB int
	// MaxBackups caps the number of rotated files retained. Zero keeps all.
	MaxBackups int
	// MaxAgeDays prunes rotated files older than this. Zero disables.
	MaxAgeDays int
	// Compress gzips rotated files.
	Compress bool

	// Sinks receive each appended record as a Span, after the JSONL write.
	// A panicking or slow sink is the consumer's problem, never the log's.
	Sinks []Sink

	mu     sync.Mutex
	log    *lumberjack.Logger
	redact RedactPolicy
}

// NewWriter returns a Writer with Now set to time.Now. Rotation fields
// default to zero (lumberjack defaults apply) and can be set by the caller.
func NewWriter(path string) *Writer {
	return &Writer{
		Path: path,
		Now:  time.Now,
	}
}

// ErrPathUnset is returned when Append is called on a Writer with empty Path.
var ErrPathUnset = errors.New("audit: log path not configured")

// Append writes one record as a JSON line. Timestamp is populated from the
// Writer if unset on the Record (zero).
func (w *Writer) Append(r Record) error {
	if w.Path == "" {
		return ErrPathUnset
	}
	now := w.now()
	if r.Timestamp == 0 {
		r.Timestamp = now.Unix()
	}
	if r.ID == "" {
		id, err := newUUIDv7(now)
		if err != nil {
			return err
		}
		r.ID = id
	}

	w.applyRedaction(&r)

	if err := os.MkdirAll(filepath.Dir(w.Path), 0o700); err != nil {
		return fmt.Errorf("audit: mkdir %s: %w", filepath.Dir(w.Path), err)
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(&r); err != nil {
		return fmt.Errorf("audit: encode: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.log == nil {
		w.log = &lumberjack.Logger{
			Filename:   w.Path,
			MaxSize:    w.MaxSizeMB,
			MaxBackups: w.MaxBackups,
			MaxAge:     w.MaxAgeDays,
			Compress:   w.Compress,
			LocalTime:  true,
		}
	}
	if _, err := w.log.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("audit: write %s: %w", w.Path, err)
	}
	w.emit(r)
	return nil
}

// emit projects the written record to every sink. Runs after the durable write
// and never returns an error: telemetry cannot fail an audited invocation.
func (w *Writer) emit(r Record) {
	if len(w.Sinks) == 0 {
		return
	}
	span := r.SpanOf()
	for _, s := range w.Sinks {
		if s != nil {
			s.Emit(span)
		}
	}
}

// Close releases the underlying log file. Safe to call multiple times and
// on a Writer that was never used. Call at process exit if you want to be
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.log == nil {
		return nil
	}
	err := w.log.Close()
	w.log = nil
	return err
}

// Wrap records an invocation by running fn and logging the result. base
// supplies caller-set fields (Verb, Argv, RepoRoot, Version);
func (w *Writer) Wrap(ctx context.Context, base Record, fn func() error) error {
	return w.WrapHook(ctx, base, fn, nil)
}

// WrapHook is Wrap with an optional onComplete callback. The hook runs
// after fn returns and before the record is appended; it gets a pointer
func (w *Writer) WrapHook(ctx context.Context, base Record, fn func() error, onComplete func(*Record)) error {
	start := w.now()
	err := fn()
	rec := base
	if rec.SessionID == "" {
		rec.SessionID = resolveSessionID(ctx)
	}
	rec.Decision = DecisionAccept
	rec.ExitCode = 0
	rec.DurationMS = w.now().Sub(start).Milliseconds()
	if err != nil {
		// The taxonomy the error already carries, rather than a flat 1: a row
		// that cannot separate a refusal from a bad flag is read weeks later.
		rec.ExitCode = exitcode.Of(err)
		rec.Error = err.Error()
		if rec.ExitCode == exitcode.PolicyDenied {
			rec.Decision = DecisionReject
		}
	}
	if onComplete != nil {
		onComplete(&rec)
	}
	if aerr := w.Append(rec); aerr != nil {
		fmt.Fprintf(os.Stderr, "audit: %v\n", aerr)
	}
	return err
}

// Preflight ensures the audit directory exists with 0700 perms and that the
// target path is writable. Call at startup so a broken config blows up
func (w *Writer) Preflight() error {
	if w.Path == "" {
		return ErrPathUnset
	}
	dir := filepath.Dir(w.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("audit: mkdir %s: %w", dir, err)
	}
	// Open in append+create mode to verify the path is writable. Don't write
	// anything; just touch the file so a permission failure surfaces here.
	f, err := os.OpenFile(w.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- caller-controlled audit path
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", w.Path, err)
	}
	return f.Close()
}

func (w *Writer) now() time.Time {
	if w.Now == nil {
		return time.Now()
	}
	return w.Now()
}

// ReadAll decodes every record from r. Useful for tests and for `audit
// tail`-style verbs.
func ReadAll(r io.Reader) ([]Record, error) {
	dec := json.NewDecoder(r)
	var out []Record
	for dec.More() {
		var rec Record
		if err := dec.Decode(&rec); err != nil {
			return out, fmt.Errorf("audit: decode: %w", err)
		}
		out = append(out, rec)
	}
	return out, nil
}
