// descriptors: the spec-driven source without a cli tree, for a consumer that
// mounts operations onto something else. See docs/specverb-descriptors.md.

package specverb

import (
	"fmt"
	"net/http"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/opcore"
)

// DescriptorConfig is what resolution needs and nothing more. It names no cli
// type, so a non-cli consumer never reaches for a projection it will not use.
type DescriptorConfig struct {
	// Guardfile is the parsed L2 policy. Use ParseFile to flatten inherit first.
	Guardfile *guardfile.Guardfile

	// Spec is the raw Swagger 2.0 / OpenAPI 3.x document bytes, decompressed.
	Spec []byte

	// Providers registers the resolvers a `value <provider>` source names.
	Providers map[string]Provider

	// HTTPClient fires the live request. nil uses http.DefaultClient.
	HTTPClient *http.Client

	// BaseURL overrides the Guardfile base-url. "" uses the Guardfile value.
	BaseURL string
}

// Descriptors resolves a spec-driven Guardfile into the pair ParseInline returns.
// Deny is absence and the cli-only nodes fail closed: specverb-descriptors.md.
func Descriptors(cfg DescriptorConfig) ([]opcore.Descriptor, opcore.RuntimeConfig, error) {
	if cfg.Guardfile == nil {
		return nil, opcore.RuntimeConfig{}, fmt.Errorf("specverb: DescriptorConfig.Guardfile is nil")
	}
	gf := cfg.Guardfile
	if len(gf.Group) == 0 {
		return nil, opcore.RuntimeConfig{}, fmt.Errorf("specverb: Guardfile has no command group")
	}
	// An action shadows a generated leaf, so dropping one serves what it replaced.
	if n := len(gf.Actions); n > 0 {
		return nil, opcore.RuntimeConfig{}, fmt.Errorf(
			"specverb: Descriptors cannot project %d `action` node(s): a cli step composite has no descriptor form, and one mounted over a generated leaf would otherwise serve the leaf it replaced", n)
	}
	if n := len(gf.Fetches); n > 0 {
		return nil, opcore.RuntimeConfig{}, fmt.Errorf(
			"specverb: Descriptors cannot project %d `fetch` node(s): a cli overlay group has no descriptor form", n)
	}

	spec, err := parseSwagger(cfg.Spec)
	if err != nil {
		return nil, opcore.RuntimeConfig{}, err
	}
	gf, err = expandWildcards(spec, gf)
	if err != nil {
		return nil, opcore.RuntimeConfig{}, err
	}
	descs, err := resolveDescriptors(spec, gf)
	if err != nil {
		return nil, opcore.RuntimeConfig{}, err
	}
	if len(descs) == 0 {
		return nil, opcore.RuntimeConfig{}, fmt.Errorf("specverb: Guardfile mounted no verbs (no `can` grants resolved)")
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = gf.BaseURL
	}
	// Descriptors without this config would fire unauthenticated and ungated.
	return descs, opcore.RuntimeConfig{
		BaseURL:      baseURL,
		Auth:         gf.Auth,
		Providers:    mergeProviders(cfg.Providers),
		Client:       cfg.HTTPClient,
		Restrict:     gf.Restrict,
		AllowMeta:    gf.AllowMeta,
		BaseURLValue: gf.BaseURLValue,
	}, nil
}
