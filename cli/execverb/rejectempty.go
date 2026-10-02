// Exec-dialect `reject-empty`: an action answering with nothing is refused
// rather than returned. See docs/execverb-occlusion.md.

package execverb

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/coilyco/umbra/pkg/exitcode"
)

// emptyAnswer reports whether output carries nothing a reader could use: no
// bytes, whitespace, or JSON null, "", [] or {}. `false` and `0` are answers.
func emptyAnswer(raw []byte) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return false
	}
	return emptyJSON(decoded)
}

// emptyJSON reads one decoded value: only the container and string cases are
// empty, which is what keeps `false` and `0` real answers.
func emptyJSON(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return len(bytes.TrimSpace([]byte(typed))) == 0
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

// applyRejectEmpty refuses an empty answer, before fail-when so the precise
// reason wins. Why it is not fail-when: docs/execverb-occlusion.md.
func applyRejectEmpty(ea execAction, lastRaw []byte) error {
	if !ea.RejectEmpty || !emptyAnswer(lastRaw) {
		return nil
	}
	return exitcode.New(exitcode.Generic, "empty_answer",
		fmt.Errorf("action %q answered with nothing, and it is declared `reject-empty`", ea.Name),
		"treat this as no result rather than as an empty one")
}
