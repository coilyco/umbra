package specverb

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/pkg/exitcode"
	"github.com/coilyco/umbra/pkg/negcontrol"
	"github.com/urfave/cli/v3"
)

// NegativeControls invokes every `never`/`cannot` grant in gf, sending and
// resolving nothing, then again with each removed. See docs/negative-controls.md.
func NegativeControls(ctx context.Context, gf *guardfile.Guardfile, spec []byte) ([]negcontrol.Control, error) {
	var out []negcontrol.Control
	for i, g := range gf.Grants {
		if g.Modal != "cannot" && g.Modal != "never" {
			continue
		}
		argv := append(append([]string{}, gf.Group...), g.Resource, g.Verb)
		got, err := invokeSpec(ctx, gf, spec, argv)
		if err != nil {
			return nil, err
		}
		without := *gf
		without.Grants = append(append([]guardfile.Grant{}, gf.Grants[:i]...), gf.Grants[i+1:]...)
		gone, err := invokeSpec(ctx, &without, spec, argv)
		if err != nil {
			gone = negcontrol.Outcome{Code: -1, Text: "mount refused: " + err.Error()}
		}
		want := denyError(denyMessage(g)).Error()
		out = append(out, negcontrol.Judge("spec "+g.Modal, g.Resource+" "+g.Verb, argv, got, gone, want, exitcode.PolicyDenied))
	}
	return out, nil
}

// recordingTransport answers every request with an empty 200 and notes that
// the call got that far.
type recordingTransport struct{ reached *bool }

func (r recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*r.reached = true
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte("{}"))),
		Header: http.Header{"Content-Type": {"application/json"}}, Request: req}, nil
}

func invokeSpec(ctx context.Context, gf *guardfile.Guardfile, spec []byte, argv []string) (negcontrol.Outcome, error) {
	var o negcontrol.Outcome
	providers := map[string]Provider{}
	for _, name := range gf.Providers() {
		providers[name] = func(context.Context, string) (string, error) { return "negative-control", nil }
	}
	root := &cli.Command{Name: gf.Group[0]}
	cfg := Config{Guardfile: gf, Spec: spec, Providers: providers,
		HTTPClient: &http.Client{Transport: recordingTransport{&o.Spawned}}}
	if err := Mount(root, cfg); err != nil {
		return o, err
	}
	if err := negcontrol.Run(ctx, root, argv); err != nil {
		o.Text = err.Error()
		o.Code = exitcode.Of(err)
	}
	return o, nil
}
