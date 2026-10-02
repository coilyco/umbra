package exitcode_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/coilyco/umbra/pkg/exitcode"
)

func TestOf(t *testing.T) {
	denied := exitcode.New(exitcode.PolicyDenied, "policy_denied", errors.New("refused"), "")
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, exitcode.Success},
		{"uncoded is generic", errors.New("plain"), exitcode.Generic},
		{"coded reports its own code", denied, exitcode.PolicyDenied},
		// The code has to survive wrapping, since every engine returns its
		// coded error up through at least one fmt.Errorf.
		{"wrapped coded is still found", fmt.Errorf("while calling: %w", denied), exitcode.PolicyDenied},
		{"user error", exitcode.New(exitcode.UserError, "user_error", errors.New("bad flag"), ""), exitcode.UserError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitcode.Of(c.err); got != c.want {
				t.Errorf("Of(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}
