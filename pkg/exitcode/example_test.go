package exitcode_test

import (
	"errors"
	"fmt"

	"github.com/coilyco/umbra/pkg/exitcode"
)

// Annotate any error with a public exit-code so orchestrators can
// pattern-match on $? without parsing stderr.
func ExampleNew() {
	err := exitcode.New(exitcode.PolicyDenied, "policy", errors.New("argv rejected"), "fix the input and retry")
	fmt.Printf("code=%d kind=%s err=%v\n", err.Code(), err.Kind(), err)
	// Output: code=2 kind=policy err=argv rejected
}

// From recovers a Coded annotation from a wrapped error chain. Use in
// main() to map a returned error to os.Exit.
func ExampleFrom() {
	err := exitcode.New(exitcode.UpstreamFailed, "upstream", errors.New("aws cli exited 7"), "")
	wrapped := fmt.Errorf("ssm get-parameter: %w", err)

	if coded := exitcode.From(wrapped); coded != nil {
		fmt.Println("would os.Exit:", coded.Code())
	}
	// Output: would os.Exit: 3
}
