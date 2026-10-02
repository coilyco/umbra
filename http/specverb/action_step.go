// The runtime's HTTP implementation of the stepflow transport seam: firing and
// planning one resolved action step.

package specverb

import (
	"context"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/pkg/stepflow"
	"github.com/urfave/cli/v3"
)

// Fire is the HTTP implementation of stepflow.Runner: it assembles the leaf
// request and fires it through the audited verb pipeline.
func (rt *runtime) Fire(ctx context.Context, c *cli.Command, leaf stepflow.Leaf, args []guardfile.ArgBind, resolve stepflow.Resolve, sliceOf stepflow.SliceOf) (any, []byte, error) {
	op := leafOp(leaf)
	method, url, body, contentType, err := rt.buildCallRequest(ctx, false, op, args, resolve, sliceOf)
	if err != nil {
		return nil, nil, err
	}
	return rt.fireCallAudited(ctx, op, method, url, body, contentType, c)
}

// Plan is the HTTP implementation of the dry-run seam: it builds the request
// offline and renders it, resolving no secret and firing nothing.
func (rt *runtime) Plan(ctx context.Context, leaf stepflow.Leaf, args []guardfile.ArgBind, resolve stepflow.Resolve, sliceOf stepflow.SliceOf) (map[string]any, error) {
	op := leafOp(leaf)
	method, url, body, contentType, err := rt.buildCallRequest(ctx, true, op, args, resolve, sliceOf)
	if err != nil {
		return nil, err
	}
	return rt.leafPlan(op, method, url, body, contentType), nil
}
