package opcore

import (
	"fmt"
	"path/filepath"

	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/policy"
)

// CheckRestrictions enforces every wrap-level restriction against a leaf's bound
// path values, failing closed on an out-of-scope value. See docs/specverb.md.
func (rt *Runtime) CheckRestrictions(pathParams, values []string) error {
	for _, r := range rt.Restrict {
		for i, p := range pathParams {
			if p != r.Param || i >= len(values) {
				continue
			}
			if !MatchesAnyGlob(values[i], r.Globs) {
				return exitcode.New(exitcode.PolicyDenied, "policy_denied",
					fmt.Errorf("argument %s=%q is outside the allowed scope (restrict %s matches %v)", p, values[i], r.Param, r.Globs),
					"supply a value within the restricted scope, or widen the `restrict` clause")
			}
		}
	}
	return nil
}

// gatePathValues gates a leaf's bound path values, skipping any param the wrap
// named in `allow-metacharacters`. See docs/specverb-request.md.
func (rt *Runtime) gatePathValues(pathParams, values []string) error {
	for i, v := range values {
		if i < len(pathParams) && rt.MetaAllowed(pathParams[i]) {
			continue
		}
		if err := policy.ValidateArg(fmt.Sprintf("positional[%d]", i), v); err != nil {
			return gateDenied(err)
		}
	}
	return nil
}

// MetaAllowed reports whether the wrap opted this path param out of the gate.
// Exported so the CLI layer's pre-gate agrees with opcore's rather than diverging.
func (rt *Runtime) MetaAllowed(param string) bool {
	for _, p := range rt.AllowMeta {
		if p == param {
			return true
		}
	}
	return false
}

// MatchesAnyGlob reports whether val matches at least one filepath.Match glob.
// A malformed pattern matches nothing (fail closed), never errors out.
func MatchesAnyGlob(val string, globs []string) bool {
	for _, g := range globs {
		if ok, err := filepath.Match(g, val); err == nil && ok {
			return true
		}
	}
	return false
}
