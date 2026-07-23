package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"forgejo.coilysiren.me/coilyco-flight-deck/cli-guard/http/kdlspecs"
)

func TestVersionReportsDriverAndDefaultCLIGuardRef(t *testing.T) {
	cmd := app()
	var out bytes.Buffer
	cmd.Writer = &out

	if err := cmd.Run(context.Background(), []string{"kdl-specs", "--version"}); err != nil {
		t.Fatalf("run --version: %v", err)
	}

	want := fmt.Sprintf(
		"kdl-specs version %s (cli-guard ref %s)\n",
		kdlspecs.DriverVersion(),
		kdlspecs.DefaultCLIGuardRef(),
	)
	if got := out.String(); got != want {
		t.Fatalf("--version output = %q, want %q", got, want)
	}
}
