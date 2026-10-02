package mcpverb_test

import (
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/mcpverb"
)

func parseConnect(t *testing.T, sentence string) (*mcpverb.Guardfile, error) {
	t.Helper()
	return mcpverb.Parse([]byte(`wrap aosguard ops fixture {
    mcp stdio { command "npx" }
    can call show { widget { can call show
        ` + sentence + `
    } }
}`))
}

func TestConnectDeclaresACSPSource(t *testing.T) {
	gf, err := parseConnect(t, `can connect "https://api.example.com"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := gf.Grants[0].Widget.Connects
	if len(got) != 1 || got[0] != "https://api.example.com" {
		t.Fatalf("Connects = %v", got)
	}
}

// Accepting a deny modal silently would leave the origin reachable while the
// guardfile reads as a block. See docs/mcpapps.md.
func TestADenyConnectIsRefusedRatherThanIgnored(t *testing.T) {
	for _, modal := range []string{"never", "cannot"} {
		t.Run(modal, func(t *testing.T) {
			_, err := parseConnect(t, modal+` connect "https://evil.example.com"`)
			if err == nil {
				t.Fatalf("`%s connect` was accepted, so the origin is reachable while the rule reads as a block", modal)
			}
			if !strings.Contains(err.Error(), "allowlist") {
				t.Fatalf("the refusal should say why it cannot be honoured: %v", err)
			}
		})
	}
}

func TestConnectRefusesWhatIsNotASourceExpression(t *testing.T) {
	cases := map[string]string{
		"directive separator": `can connect "https://a.example.com; script-src *"`,
		"whitespace":          `can connect "https://a.example.com https://b.example.com"`,
		"a regex":             `can connect "^https://api\\."`,
		"empty":               `can connect ""`,
	}
	for name, sentence := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConnect(t, sentence); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestWidgetPolicyCarriesTheDeclaredSourcesToTheCSP(t *testing.T) {
	gf, err := parseConnect(t, `can connect "https://api.example.com"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := gf.Grants[0].Widget.Connects; len(got) != 1 {
		t.Fatalf("the block lost its source: %v", got)
	}
}
