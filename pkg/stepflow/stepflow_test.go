package stepflow

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/urfave/cli/v3"
)

type fakeLeaf string

func (l fakeLeaf) Label() string { return string(l) }

type firedStep struct {
	leaf string
	args []string
}

type fakeRunner struct {
	fired []firedStep
	fail  map[string]error
}

func (r *fakeRunner) Fire(_ context.Context, _ *cli.Command, leaf Leaf, args []guardfile.ArgBind, resolve Resolve, _ SliceOf) (any, []byte, error) {
	values := make([]string, 0, len(args))
	for _, arg := range args {
		v, err := resolve(arg.Value)
		if err != nil {
			return nil, nil, err
		}
		values = append(values, v)
	}
	r.fired = append(r.fired, firedStep{leaf: leaf.Label(), args: values})
	if err := r.fail[leaf.Label()]; err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": float64(len(r.fired)), "value": leaf.Label()}, []byte(leaf.Label()), nil
}

func (r *fakeRunner) Plan(_ context.Context, leaf Leaf, args []guardfile.ArgBind, resolve Resolve, _ SliceOf) (map[string]any, error) {
	values := make([]string, 0, len(args))
	for _, arg := range args {
		v, err := resolve(arg.Value)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return map[string]any{"leaf": leaf.Label(), "args": values}, nil
}

func TestRunPreservesOrderAndThreadsBindings(t *testing.T) {
	r := &fakeRunner{}
	steps := []Step{
		{Leaf: fakeLeaf("create"), As: "created"},
		{Leaf: fakeLeaf("use"), Args: []guardfile.ArgBind{{Name: "id", Value: "$created.id"}}, As: "used"},
	}
	bindings, raw, err := Run(context.Background(), nil, steps, map[string]string{}, nil, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := r.fired, []firedStep{{leaf: "create", args: []string{}}, {leaf: "use", args: []string{"1"}}}; !reflect.DeepEqual(got, want) {
		t.Errorf("fire order/args = %#v, want %#v", got, want)
	}
	if bindings["used"] == nil || string(raw) != "use" {
		t.Errorf("bindings/raw = %#v/%q, want final binding and raw output", bindings, raw)
	}
}

func TestRunStopsAtFailureWithoutRunningLaterSteps(t *testing.T) {
	r := &fakeRunner{fail: map[string]error{"second": errors.New("nope")}}
	steps := []Step{{Leaf: fakeLeaf("first")}, {Leaf: fakeLeaf("second")}, {Leaf: fakeLeaf("third")}}
	_, _, err := Run(context.Background(), nil, steps, map[string]string{}, nil, r)
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if got, want := r.fired, []firedStep{{leaf: "first", args: []string{}}, {leaf: "second", args: []string{}}}; !reflect.DeepEqual(got, want) {
		t.Errorf("fired = %#v, want %#v", got, want)
	}
}

func TestPlanCallsThreadsAsWithoutDeploymentFields(t *testing.T) {
	r := &fakeRunner{}
	plan, err := PlanCalls(context.Background(), []Step{{Leaf: fakeLeaf("first"), As: "first"}}, func(v string) (string, error) { return v, nil }, nil, r)
	if err != nil {
		t.Fatalf("PlanCalls: %v", err)
	}
	entry := plan[0].(map[string]any)
	if entry["as"] != "first" || entry["leaf"] != "first" {
		t.Errorf("plan = %#v, want leaf and as", entry)
	}
}
