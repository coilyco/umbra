package skillgen_test

import (
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/skillgen"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

func sampleTree() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "aws",
			Usage: "Pass-through to aws.",
			Commands: []*cli.Command{
				{
					Name:  "ssm",
					Usage: "ssm",
					Commands: []*cli.Command{
						{
							Name:  "get-parameter",
							Usage: "Read one parameter.",
							Flags: []cli.Flag{
								&cli.StringFlag{Name: "name"},
								&cli.BoolFlag{Name: "with-decryption"},
							},
						},
					},
				},
			},
		},
	}
}

func TestRenderMarkdown_ContainsLeaf(t *testing.T) {
	body := skillgen.RenderMarkdown(sampleTree(), "tool")
	if !strings.Contains(body, "## `tool aws ssm get-parameter`") {
		t.Error("markdown body missing leaf header")
	}
	if !strings.Contains(body, "Flags: --name, --with-decryption") {
		t.Error("markdown body missing flags line")
	}
}

func TestRenderMarkdown_HonorsRootName(t *testing.T) {
	body := skillgen.RenderMarkdown(sampleTree(), "tool")
	if !strings.Contains(body, "## `tool aws ssm get-parameter`") {
		t.Errorf("root prefix not applied; got: %s", body)
	}
}

func TestRenderYAML_StructuredShape(t *testing.T) {
	body, err := skillgen.RenderYAML(sampleTree(), "tool")
	if err != nil {
		t.Fatalf("RenderYAML: %v", err)
	}
	var parsed struct {
		Commands []skillgen.Entry `yaml:"commands"`
	}
	if err := yaml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Commands) != 1 {
		t.Fatalf("got %d commands, want 1", len(parsed.Commands))
	}
	got := parsed.Commands[0]
	wantPath := []string{"tool", "aws", "ssm", "get-parameter"}
	if strings.Join(got.Path, ".") != strings.Join(wantPath, ".") {
		t.Errorf("path = %v, want %v", got.Path, wantPath)
	}
	if got.Summary != "Read one parameter." {
		t.Errorf("summary = %q", got.Summary)
	}
	if len(got.Flags) != 2 {
		t.Errorf("flags = %v, want 2", got.Flags)
	}
}

func TestRenderSkillBuildsConciseNativeBundle(t *testing.T) {
	first, err := skillgen.RenderSkill(sampleTree(), "Tool Guard")
	if err != nil {
		t.Fatalf("RenderSkill: %v", err)
	}
	second, err := skillgen.RenderSkill(sampleTree(), "Tool Guard")
	if err != nil {
		t.Fatalf("RenderSkill second pass: %v", err)
	}
	if first != second {
		t.Fatal("RenderSkill is not deterministic")
	}
	if first.Name != "tool-guard" {
		t.Errorf("skill name = %q, want tool-guard", first.Name)
	}
	for _, want := range []string{
		"name: tool-guard",
		"description: Use the guarded Tool Guard CLI",
		"`Tool Guard --help`",
		"`references/commands.yaml`",
	} {
		if !strings.Contains(first.Skill, want) {
			t.Errorf("SKILL.md missing %q:\n%s", want, first.Skill)
		}
	}
	if strings.Contains(first.Skill, "get-parameter") {
		t.Errorf("SKILL.md copied exhaustive leaf help:\n%s", first.Skill)
	}
	var parsed struct {
		Commands []skillgen.Entry `yaml:"commands"`
	}
	if err := yaml.Unmarshal([]byte(first.CommandsYAML), &parsed); err != nil {
		t.Fatalf("parse generated command index: %v", err)
	}
	if len(parsed.Commands) != 1 || strings.Join(parsed.Commands[0].Path, " ") != "Tool Guard aws ssm get-parameter" {
		t.Errorf("command index missing reachable leaf: %+v", parsed.Commands)
	}
}

func TestRenderSkillRejectsEmptyRoot(t *testing.T) {
	if _, err := skillgen.RenderSkill(sampleTree(), " "); err == nil {
		t.Fatal("RenderSkill accepted an empty root")
	}
}
