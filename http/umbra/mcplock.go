package umbra

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/mcpverb"
	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/pkg/mcpclient"
	"github.com/coilyco/umbra/pkg/valuesource"
)

// mcpLockTimeout bounds the one online step an mcp member takes. A stdio server
// that never completes initialize would otherwise hang `lock` with no output.
const mcpLockTimeout = 60 * time.Second

// fetchTools connects to the member's declared upstream and reads its whole
// tool surface: this dialect's fetchSpec, and its only online step.
func fetchTools(m member) ([]mcpclient.Tool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpLockTimeout)
	defer cancel()

	server, err := lockServer(ctx, m.MCPGF)
	if err != nil {
		return nil, err
	}
	sess, err := mcpclient.Connect(ctx, server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	return sess.ListTools(ctx)
}

// lockServer resolves the guardfile's declared upstream into a connectable one.
func lockServer(ctx context.Context, gf *mcpverb.Guardfile) (mcpclient.Server, error) {
	s := gf.Server
	if s.Kind == "stdio" {
		env := make([]string, 0, len(s.Env))
		for _, e := range s.Env {
			v, err := resolveLockValue(ctx, e.Value)
			if err != nil {
				return mcpclient.Server{}, fmt.Errorf("resolve env %s: %w", e.Name, err)
			}
			env = append(env, e.Name+"="+v)
		}
		return mcpclient.Server{
			Name:  s.Command,
			Stdio: &mcpclient.Stdio{Command: s.Command, Argv: s.Argv, Env: env},
		}, nil
	}
	url := s.URL
	if url == "" {
		v, err := resolveLockValue(ctx, s.URLValue)
		if err != nil {
			return mcpclient.Server{}, fmt.Errorf("resolve url: %w", err)
		}
		url = v
	}
	headers := map[string]string{}
	if !s.Auth.Value.IsZero() {
		token, err := resolveLockValue(ctx, s.Auth.Value)
		if err != nil {
			return mcpclient.Server{}, fmt.Errorf("resolve auth: %w", err)
		}
		header, prefix := s.Auth.Header, s.Auth.Prefix
		if s.Auth.Scheme == "bearer" {
			header, prefix = "Authorization", "Bearer "
		}
		if header == "" {
			header = "Authorization"
		}
		headers[header] = prefix + token
	}
	return mcpclient.Server{Name: url, HTTP: &mcpclient.HTTPEndpoint{URL: url, Headers: headers}}, nil
}

// resolveLockValue reads the first source in a chain that yields a value.
func resolveLockValue(ctx context.Context, chain guardfile.ValueChain) (string, error) {
	return valuesource.ResolveFirst(ctx, nil, opcore.ChainSources(chain))
}

// pruneTools keeps only the tools the guardfile actually grants, so the lock is
// the consumer's own contract rather than a dump of everything upstream offers.
func pruneTools(gf *mcpverb.Guardfile, tools []mcpclient.Tool) ([]mcpclient.Tool, error) {
	names := make([]string, 0, len(tools))
	byName := map[string]mcpclient.Tool{}
	for _, t := range tools {
		names = append(names, t.Name)
		byName[t.Name] = t
	}
	grants, err := gf.Granted(names)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]mcpclient.Tool, 0, len(grants))
	for _, g := range grants {
		if seen[g.Tool] {
			continue
		}
		seen[g.Tool] = true
		out = append(out, byName[g.Tool])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// encodeTools renders the pruned surface as the lock's decoded form.
func encodeTools(tools []mcpclient.Tool) ([]byte, error) {
	b, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode tool lock: %w", err)
	}
	return append(b, '\n'), nil
}

// decodeTools reads a committed tool lock.
func decodeTools(b []byte) ([]mcpclient.Tool, error) {
	var tools []mcpclient.Tool
	if err := json.Unmarshal(b, &tools); err != nil {
		return nil, fmt.Errorf("decode tool lock: %w", err)
	}
	return tools, nil
}

// diffTools reports drift between a committed tool lock and live upstream: a
// tool gone, one appeared, or one whose schema or `_meta` moved.
func diffTools(committed, live []mcpclient.Tool) ([]string, error) {
	was := byName(committed)
	now := byName(live)
	var drift []string
	for _, n := range unionNames(was, now) {
		prior, hadPrior := was[n]
		current, hasCurrent := now[n]
		switch {
		case !hasCurrent:
			drift = append(drift, "tool removed upstream: "+n)
		case !hadPrior:
			drift = append(drift, "tool added upstream: "+n)
		default:
			changed, err := toolChanged(prior, current)
			if err != nil {
				return nil, err
			}
			if changed != "" {
				drift = append(drift, "tool "+n+" changed: "+changed)
			}
		}
	}
	return drift, nil
}

// byName indexes a tool surface for comparison.
func byName(tools []mcpclient.Tool) map[string]mcpclient.Tool {
	out := make(map[string]mcpclient.Tool, len(tools))
	for _, t := range tools {
		out[t.Name] = t
	}
	return out
}

// unionNames lists every tool in either surface, sorted so drift reports in a
// stable order rather than map order.
func unionNames(was, now map[string]mcpclient.Tool) []string {
	names := map[string]bool{}
	for n := range was {
		names[n] = true
	}
	for n := range now {
		names[n] = true
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// toolChanged names which part of a tool moved, empty when nothing did.
func toolChanged(prior, current mcpclient.Tool) (string, error) {
	var changed []string
	for _, part := range []struct {
		name       string
		was, isNow any
	}{
		{"input schema", prior.InputSchema, current.InputSchema},
		{"output schema", prior.OutputSchema, current.OutputSchema},
		{"_meta", prior.Meta, current.Meta},
		{"annotations", prior.Annotations, current.Annotations},
		{"description", prior.Description, current.Description},
		{"title", prior.Title, current.Title},
	} {
		same, err := jsonEqual(part.was, part.isNow)
		if err != nil {
			return "", err
		}
		if !same {
			changed = append(changed, part.name)
		}
	}
	if len(changed) == 0 {
		return "", nil
	}
	out := changed[0]
	for _, c := range changed[1:] {
		out += ", " + c
	}
	return out, nil
}

// jsonEqual compares two values by their canonical JSON encoding.
func jsonEqual(a, b any) (bool, error) {
	ab, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return string(ab) == string(bb), nil
}
