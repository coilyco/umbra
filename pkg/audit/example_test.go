package audit_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/coilyco/umbra/pkg/audit"
)

// The most basic shape: open a writer, append one record, close.
// The writer creates and rotates the JSONL file under the hood.
func ExampleNewWriter() {
	path := filepath.Join(os.TempDir(), "umbra-example.jsonl")
	w := audit.NewWriter(path)
	defer func() { _ = w.Close() }()

	if err := w.Preflight(); err != nil {
		fmt.Println("preflight:", err)
		return
	}

	rec := audit.Record{Verb: "hello", Argv: []string{"hello", "world"}}
	if err := w.Append(rec); err != nil {
		fmt.Println("append:", err)
		return
	}
	fmt.Println("ok")
	// Output: ok
}

// Wrap runs a function and records one audit row per invocation,
// regardless of whether the function returns an error.
func ExampleWriter_Wrap() {
	w := audit.NewWriter(filepath.Join(os.TempDir(), "umbra-wrap-example.jsonl"))
	defer func() { _ = w.Close() }()
	_ = w.Preflight()

	err := w.Wrap(context.Background(), audit.Record{Verb: "demo"}, func() error {
		fmt.Println("doing work")
		return nil
	})
	fmt.Println("err:", err)
	// Output: doing work
	// err: <nil>
}
