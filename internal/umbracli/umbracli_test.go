package umbracli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/umbra"
)

func TestVersionReportsDriverAndDefaultCLIGuardRef(t *testing.T) {
	var out bytes.Buffer

	if code := Run(context.Background(), []string{"umbra", "--version"}, &out, &out); code != 0 {
		t.Fatalf("run --version exit code = %d, want 0", code)
	}

	want := fmt.Sprintf(
		"umbra version %s (umbra ref %s)\n",
		umbra.DriverVersion(),
		umbra.DefaultCLIGuardRef(),
	)
	if got := out.String(); got != want {
		t.Fatalf("--version output = %q, want %q", got, want)
	}
}

func TestHelpExposesExplicitSkillOutputRoot(t *testing.T) {
	var out bytes.Buffer

	if code := Run(context.Background(), []string{"umbra", "--help"}, &out, &out); code != 0 {
		t.Fatalf("run --help exit code = %d, want 0", code)
	}
	for _, want := range []string{"--skills-out string", "<root>/<binary>/SKILL.md", "references/commands.yaml"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--help missing %q:\n%s", want, out.String())
		}
	}
}
