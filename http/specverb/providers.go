package specverb

import (
	"github.com/coilyco/umbra/pkg/valuesource"
)

// Provider resolves the value at address for one named value source: the shared
// valuesource.Provider, so the consumer's one registry drives both engines.
type Provider = valuesource.Provider

// mergeProviders layers the consumer's registry over umbra's no-SDK built-ins
// (env, file, literal; consumer wins on a clash). A missing one fails in resolveValue.
func mergeProviders(consumer map[string]Provider) map[string]Provider {
	return valuesource.Merge(consumer)
}
