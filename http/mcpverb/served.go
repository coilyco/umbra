package mcpverb

import (
	"encoding/json"
	"fmt"

	"github.com/coilyco/umbra/http/opcore"
)

// ServedTool is one granted tool as a server would advertise it, plus the
// descriptor that executes a call. See docs/mcpverb-serving.md.
type ServedTool struct {
	Name        string
	Title       string
	Description string

	// InputSchema is a draft-07 object built from the committed lock: what is
	// advertised is what was granted, not what upstream has.
	InputSchema json.RawMessage

	// Meta is upstream `_meta`, forwarded verbatim and never parsed, so an MCP
	// Apps widget address survives the hop.
	Meta map[string]any

	// Descriptor executes through opcore.Operation, so a served call passes the
	// same guards a CLI leaf does.
	Descriptor opcore.Descriptor
}

// ServedTools projects the granted surface into tool definitions plus the
// runtime that fires them. See docs/mcpverb-serving.md.
func ServedTools(cfg Config) ([]ServedTool, opcore.RuntimeConfig, error) {
	descs, rt, err := Descriptors(cfg)
	if err != nil {
		return nil, opcore.RuntimeConfig{}, err
	}
	out := make([]ServedTool, 0, len(descs))
	for _, d := range descs {
		if d.MCP == nil {
			return nil, opcore.RuntimeConfig{}, fmt.Errorf("mcpverb: descriptor %q is not an mcp leaf", d.Leaf)
		}
		out = append(out, ServedTool{
			// The upstream name, not the CLI leaf: a served surface speaks the
			// protocol's namespace rather than a shell one.
			Name:        d.MCP.Tool,
			Description: d.Describe,
			InputSchema: d.InputSchema().JSONSchema(),
			Meta:        d.Meta,
			Descriptor:  d,
		})
	}
	return out, rt, nil
}
