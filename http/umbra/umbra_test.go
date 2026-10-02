package umbra

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	kdl "github.com/calico32/kdl-go"

	"github.com/coilyco/umbra/http/umbra/codegen"
	"gopkg.in/yaml.v3"
)

const guardfileFixture = `wrap ward-kdl ops forgejo {
	spec forgejo.swagger.v1.json
	base-url "forgejo.example/api/v1"
	auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
	can read repos { op "repoGet" }
	can create repos { op "createCurrentUserRepo" }
}`

// execFixture is an exec-dialect member sharing the ward-kdl binary with the
// spec fixture above, so the two merge into one binary.
const execFixture = `wrap ward-kdl ops aws {
	exec aws
	can run sts get-caller-identity
	can run s3 ls {
		deny-when arg0 matches "*tfstate*"
	}
}`

const embeddedExecFixture = `wrap ward-kdl ops measure {
	exec python3
	can run storage {
		argv "-I"
		embed "scripts/storage_measure.py"
		sealed
	}
}`

const skillSpecFixture = `{
	"swagger": "2.0",
	"info": {"title": "test", "version": "1"},
	"paths": {
		"/repos/{owner}/{repo}": {
			"get": {
				"operationId": "repoGet",
				"parameters": [
					{"name": "owner", "in": "path", "required": true, "type": "string"},
					{"name": "repo", "in": "path", "required": true, "type": "string"}
				],
				"responses": {"200": {"description": "ok"}}
			}
		},
		"/user/repos": {
			"post": {
				"operationId": "createCurrentUserRepo",
				"responses": {"201": {"description": "created"}}
			}
		}
	}
}`

func TestSniffTransport(t *testing.T) {
	spec, err := sniffTransport([]byte(guardfileFixture))
	if err != nil || spec != codegen.TransportSpec {
		t.Errorf("spec fixture: got (%q, %v), want spec", spec, err)
	}
	ex, err := sniffTransport([]byte(execFixture))
	if err != nil || ex != codegen.TransportExec {
		t.Errorf("exec fixture: got (%q, %v), want exec", ex, err)
	}
}

func TestReadMemberDispatchesExec(t *testing.T) {
	dir := t.TempDir()
	gfPath := filepath.Join(dir, "aws.guardfile.kdl")
	if err := os.WriteFile(gfPath, []byte(execFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := readMember(gfPath, filepath.Base(gfPath))
	if err != nil {
		t.Fatalf("readMember: %v", err)
	}
	if !m.isExec() {
		t.Errorf("want exec member, got transport %q", m.Params.Transport)
	}
	if m.ExecGF == nil || m.GF != nil {
		t.Errorf("exec member should carry ExecGF and no spec GF (GF=%v ExecGF=%v)", m.GF, m.ExecGF)
	}
	if m.Params.Binary != "ward-kdl" {
		t.Errorf("binary: got %q want ward-kdl", m.Params.Binary)
	}
	if m.Params.SpecLockName != "" || m.Params.SpecURL != "" {
		t.Errorf("exec member should have no spec lock/url, got %+v", m.Params)
	}
}

func TestReadMemberLoadsEmbeddedFile(t *testing.T) {
	dir := t.TempDir()
	writeMember(t, dir, "ops/scripts/storage_measure.py", "print('measured')\n")
	gfPath := writeMember(t, dir, "ops/measure.kdl", embeddedExecFixture)
	m, err := readMember(gfPath, "ops/measure.kdl")
	if err != nil {
		t.Fatalf("readMember: %v", err)
	}
	if len(m.Embeds) != 1 {
		t.Fatalf("Embeds = %+v", m.Embeds)
	}
	embedded := m.Embeds[0]
	if embedded.Source != "scripts/storage_measure.py" || embedded.Name != "ops/scripts/storage_measure.py" || string(embedded.Bytes) != "print('measured')\n" {
		t.Errorf("embedded file = %+v", embedded)
	}
	want := []codegen.EmbeddedFile{{Source: "scripts/storage_measure.py", Name: "ops/scripts/storage_measure.py"}}
	if !reflect.DeepEqual(m.Params.EmbeddedFiles, want) {
		t.Errorf("Params.EmbeddedFiles = %+v, want %+v", m.Params.EmbeddedFiles, want)
	}
}

func TestReadMemberRejectsInvalidEmbeddedFile(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir := t.TempDir()
		gfPath := writeMember(t, dir, "measure.kdl", embeddedExecFixture)
		if _, err := readMember(gfPath, "measure.kdl"); err == nil || !strings.Contains(err.Error(), "resolve embedded file") {
			t.Fatalf("missing embedded file error = %v", err)
		}
	})

	t.Run("non-regular", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "scripts", "storage_measure.py"), 0o750); err != nil {
			t.Fatal(err)
		}
		gfPath := writeMember(t, dir, "measure.kdl", embeddedExecFixture)
		if _, err := readMember(gfPath, "measure.kdl"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("non-regular embedded file error = %v", err)
		}
	})

	t.Run("symlink escape", func(t *testing.T) {
		dir := t.TempDir()
		outside := writeMember(t, t.TempDir(), "storage_measure.py", "print('outside')\n")
		if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "scripts", "storage_measure.py")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		gfPath := writeMember(t, dir, "measure.kdl", embeddedExecFixture)
		if _, err := readMember(gfPath, "measure.kdl"); err == nil || !strings.Contains(err.Error(), "escapes guardfile directory") {
			t.Fatalf("symlink escape error = %v", err)
		}
	})

	t.Run("too large", func(t *testing.T) {
		dir := t.TempDir()
		script := writeMember(t, dir, "scripts/storage_measure.py", "")
		if err := os.Truncate(script, maxEmbeddedFileBytes+1); err != nil {
			t.Fatal(err)
		}
		gfPath := writeMember(t, dir, "measure.kdl", embeddedExecFixture)
		if _, err := readMember(gfPath, "measure.kdl"); err == nil || !strings.Contains(err.Error(), "above the") {
			t.Fatalf("oversized embedded file error = %v", err)
		}
	})
}

func TestEmbeddedFileBytesInvalidateMemberHash(t *testing.T) {
	dir := t.TempDir()
	script := writeMember(t, dir, "scripts/storage_measure.py", "print('one')\n")
	gfPath := writeMember(t, dir, "measure.kdl", embeddedExecFixture)
	first, err := readMember(gfPath, "measure.kdl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("print('two')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := readMember(gfPath, "measure.kdl")
	if err != nil {
		t.Fatal(err)
	}
	if hashMembers([]member{first}) == hashMembers([]member{second}) {
		t.Fatal("embedded source change did not invalidate the member hash")
	}
}

func writeMember(t *testing.T, root, name, src string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectRootDiscoversArbitraryNestedMembersInStableOrder(t *testing.T) {
	dir := t.TempDir()
	writeMember(t, dir, "forge/writes.kdl", guardfileFixture)
	selected := writeMember(t, dir, "cloud/reads.kdl", guardfileFixture)
	// A fleet configuration is recognized as unrelated project KDL and ignored.
	writeMember(t, dir, "fleet.kdl", `agents { schema-version 2; agent codex { binary codex } }`)
	g, err := loadGroup(Options{ProjectRoot: dir, GuardfilePath: selected})
	if err != nil {
		t.Fatalf("loadGroup: %v", err)
	}
	if got, want := []string{g.Members[0].Path, g.Members[1].Path}, []string{"cloud/reads.kdl", "forge/writes.kdl"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("member order = %v, want %v", got, want)
	}
	for _, m := range g.Members {
		if m.Params.GuardfileName != m.Path {
			t.Errorf("embed identity = %q, want %q", m.Params.GuardfileName, m.Path)
		}
	}
	main, err := g.render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"//go:embed cloud/reads.kdl", "//go:embed forge/writes.kdl"} {
		if !strings.Contains(string(main), want) {
			t.Errorf("rendered source missing %q", want)
		}
	}
}

func TestConventionalProjectRootDiscoversDotUmbraRecursively(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, ".umbra")
	writeMember(t, project, "aguard/ops.kdl", guardfileFixture)
	t.Chdir(dir)

	g, err := loadGroup(Options{})
	if err != nil {
		t.Fatalf("loadGroup: %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if g.Dir != wantRoot {
		t.Errorf("project root = %q, want %q", g.Dir, wantRoot)
	}
	if len(g.Members) != 1 || g.Members[0].Path != "aguard/ops.kdl" {
		t.Errorf("conventional members = %+v", g.Members)
	}
}

func TestExplicitGuardfileKeepsLegacyDiscoveryBesideDotUmbra(t *testing.T) {
	dir := t.TempDir()
	writeMember(t, filepath.Join(dir, ".umbra"), "aguard/ops.kdl", guardfileFixture)
	selected := writeMember(t, dir, "legacy.guardfile.kdl", strings.Replace(guardfileFixture, "wrap ward-kdl", "wrap legacy", 1))
	t.Chdir(dir)

	g, err := loadGroup(Options{GuardfilePath: selected})
	if err != nil {
		t.Fatalf("loadGroup: %v", err)
	}
	if g.Binary != "legacy" || len(g.Members) != 1 || g.Members[0].Path != "legacy.guardfile.kdl" {
		t.Errorf("legacy selection = %+v", g)
	}
}

func TestProjectRootUsesDistinctArtifactsForSameBasename(t *testing.T) {
	dir := t.TempDir()
	writeMember(t, dir, "cloud/api.kdl", strings.Replace(guardfileFixture, "ops forgejo", "ops cloud", 1))
	selected := writeMember(t, dir, "forge/api.kdl", guardfileFixture)
	g, err := loadGroup(Options{ProjectRoot: dir, GuardfilePath: selected})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{g.Members[0].Params.SpecLockName, g.Members[1].Params.SpecLockName}
	want := []string{"cloud/forgejo.swagger.lock.json.gz", "forge/forgejo.swagger.lock.json.gz"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("lock names = %v, want %v", got, want)
	}
	if got[0] == got[1] {
		t.Errorf("same-basename members must keep separate lock names: %v", got)
	}
}

func TestMaterializeModuleDirPreservesNestedMemberArtifacts(t *testing.T) {
	dir := t.TempDir()
	mems := []member{
		{
			Path:   "cloud/api.kdl",
			Params: codegen.Params{GuardfileName: "cloud/api.kdl", SpecLockName: "cloud/api.lock.json.gz"},
			Bytes:  []byte("cloud"),
			Embeds: []embeddedFile{{Name: "cloud/scripts/measure.py", Bytes: []byte("measure")}},
		},
		{Path: "forge/api.kdl", Params: codegen.Params{GuardfileName: "forge/api.kdl", SpecLockName: "forge/api.lock.json.gz"}, Bytes: []byte("forge")},
	}
	if err := materializeModuleDir(dir, []byte("package main\n"), mems, map[string][]byte{"cloud/api.kdl": []byte("cloud lock"), "forge/api.kdl": []byte("forge lock")}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"cloud/api.kdl":            "cloud",
		"forge/api.kdl":            "forge",
		"cloud/api.lock.json.gz":   "cloud lock",
		"forge/api.lock.json.gz":   "forge lock",
		"cloud/scripts/measure.py": "measure",
	} {
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || string(got) != want {
			t.Errorf("artifact %s = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestProjectRootRequiresSelectorForMultipleBinaries(t *testing.T) {
	dir := t.TempDir()
	selected := writeMember(t, dir, "a.kdl", guardfileFixture)
	writeMember(t, dir, "b.kdl", strings.Replace(guardfileFixture, "wrap ward-kdl", "wrap other", 1))
	if _, err := loadGroup(Options{ProjectRoot: dir}); err == nil || !strings.Contains(err.Error(), "pass --guardfile") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("multi-binary discovery error = %v", err)
	}
	g, err := loadGroup(Options{ProjectRoot: dir, GuardfilePath: selected})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Members) != 1 || g.Binary != "ward-kdl" {
		t.Errorf("selected group = %+v", g)
	}
}

func TestProjectRootFailsClosedForConflictingMemberLock(t *testing.T) {
	dir := t.TempDir()
	selected := writeMember(t, dir, "a.kdl", guardfileFixture)
	writeMember(t, dir, "b.kdl", strings.Replace(guardfileFixture, "ops forgejo", "ops alternate", 1))
	if _, err := loadGroup(Options{ProjectRoot: dir, GuardfilePath: selected}); err == nil || !strings.Contains(err.Error(), "conflicting spec lock") {
		t.Fatalf("conflicting lock error = %v", err)
	}
}

func TestProjectRootRejectsEmbeddedArtifactCollision(t *testing.T) {
	dir := t.TempDir()
	src := strings.Replace(embeddedExecFixture, `embed "scripts/storage_measure.py"`, `embed "measure.kdl"`, 1)
	selected := writeMember(t, dir, "measure.kdl", src)
	if _, err := loadGroup(Options{ProjectRoot: dir, GuardfilePath: selected}); err == nil || !strings.Contains(err.Error(), "embedded artifact") {
		t.Fatalf("embedded artifact collision error = %v", err)
	}
}

func TestProjectRootFailsClosedForMalformedOperationAndSymlinkEscape(t *testing.T) {
	t.Run("malformed intended member", func(t *testing.T) {
		dir := t.TempDir()
		writeMember(t, dir, "broken.kdl", "wrap ward-kdl {")
		if _, err := loadGroup(Options{ProjectRoot: dir}); err == nil || !strings.Contains(err.Error(), "intended operation") {
			t.Fatalf("malformed operation error = %v", err)
		}
	})
	t.Run("symlink escape", func(t *testing.T) {
		dir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.kdl")
		if err := os.WriteFile(outside, []byte(guardfileFixture), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "escape.kdl")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := loadGroup(Options{ProjectRoot: dir}); err == nil || !strings.Contains(err.Error(), "escapes project root") {
			t.Fatalf("symlink escape error = %v", err)
		}
	})
}

func TestLoadGroupMergesSpecAndExec(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "forgejo.guardfile.kdl"), []byte(guardfileFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aws.guardfile.kdl"), []byte(execFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := loadGroup(Options{GuardfilePath: filepath.Join(dir, "forgejo.guardfile.kdl")})
	if err != nil {
		t.Fatalf("loadGroup: %v", err)
	}
	if len(g.Members) != 2 {
		t.Fatalf("want 2 merged members, got %d", len(g.Members))
	}
	main, err := g.render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "main.go", main, parser.AllErrors); err != nil {
		t.Fatalf("merged main.go does not parse: %v\n%s", err, main)
	}
	src := string(main)
	for _, want := range []string{"specverb.Mount(app", "execverb.Mount(app", "//go:embed aws.guardfile.kdl"} {
		if !strings.Contains(src, want) {
			t.Errorf("merged main.go missing %q", want)
		}
	}
}

func TestLoadGroupKeepsSourceBinaryWhenRuntimeNameChanges(t *testing.T) {
	dir := t.TempDir()
	gfPath := filepath.Join(dir, "forgejo.guardfile.kdl")
	if err := os.WriteFile(gfPath, []byte(guardfileFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	defaultGroup, err := loadGroup(Options{GuardfilePath: gfPath})
	if err != nil {
		t.Fatalf("load default group: %v", err)
	}
	renamedGroup, err := loadGroup(Options{GuardfilePath: gfPath, BinaryName: "ward"})
	if err != nil {
		t.Fatalf("load renamed group: %v", err)
	}
	if renamedGroup.Binary != "ward-kdl" {
		t.Errorf("source binary changed: got %q want ward-kdl", renamedGroup.Binary)
	}
	if renamedGroup.runtimeBinary() != "ward" {
		t.Errorf("runtime binary: got %q want ward", renamedGroup.runtimeBinary())
	}
	for _, tc := range []struct {
		goos string
		want string
	}{
		{goos: "linux", want: "ward"},
		{goos: "windows", want: "ward.exe"},
	} {
		if got := executablePathForOS(tc.goos, renamedGroup.runtimeBinary()); got != tc.want {
			t.Errorf("%s runtime executable: got %q want %q", tc.goos, got, tc.want)
		}
	}
	main, err := renamedGroup.render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	src := string(main)
	for _, want := range []string{`Name: "ward"`, `WARD_KDL_OPS_FORGEJO_SPEC`} {
		if !strings.Contains(src, want) {
			t.Errorf("renamed main.go missing %q", want)
		}
	}
	defaultKey := cacheKeyForGroup(defaultGroup)
	renamedKey := cacheKeyForGroup(renamedGroup)
	if defaultKey == renamedKey {
		t.Errorf("renamed build reused default cache key %q", defaultKey)
	}
}

func TestLoadGroupRejectsInvalidRuntimeBinaryName(t *testing.T) {
	dir := t.TempDir()
	gfPath := filepath.Join(dir, "forgejo.guardfile.kdl")
	if err := os.WriteFile(gfPath, []byte(guardfileFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{" ward", "ward ", "../ward", "bin/ward", `bin\ward`, ".", "..", "ward\x00"} {
		t.Run(name, func(t *testing.T) {
			_, err := loadGroup(Options{GuardfilePath: gfPath, BinaryName: name})
			if err == nil {
				t.Fatal("expected invalid runtime binary name to error")
			}
			if !strings.Contains(err.Error(), "--binary") {
				t.Fatalf("error should mention --binary, got %v", err)
			}
		})
	}
}

func TestGenDoesNotEmitReferenceDocs(t *testing.T) {
	dir := t.TempDir()
	gfPath := filepath.Join(dir, "aws.guardfile.kdl")
	if err := os.WriteFile(gfPath, []byte(execFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Gen(Options{GuardfilePath: gfPath, Out: filepath.Join(dir, "main.go")}); err != nil {
		t.Fatalf("Gen: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "aws.guardfile.md")); !os.IsNotExist(err) {
		t.Fatalf("Gen wrote retired reference doc: %v", err)
	}
}

func TestGenWritesDeterministicMixedTransportSkill(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "forgejo.guardfile.kdl")
	execPath := filepath.Join(dir, "aws.guardfile.kdl")
	if err := os.WriteFile(specPath, []byte(guardfileFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(execPath, []byte(execFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSpecLock(filepath.Join(dir, "forgejo.swagger.lock.json.gz"), []byte(skillSpecFixture)); err != nil {
		t.Fatal(err)
	}
	skillsOut := filepath.Join(dir, "skills")
	opts := Options{
		GuardfilePath: specPath,
		BinaryName:    "fixtureguard",
		Out:           filepath.Join(dir, "main.go"),
		SkillsOut:     skillsOut,
	}
	if err := Gen(opts); err != nil {
		t.Fatalf("Gen: %v", err)
	}

	skillPath := filepath.Join(skillsOut, "fixtureguard", "SKILL.md")
	indexPath := filepath.Join(skillsOut, "fixtureguard", "references", "commands.yaml")
	firstSkill, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	firstIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read commands.yaml: %v", err)
	}
	if err := Gen(opts); err != nil {
		t.Fatalf("Gen second pass: %v", err)
	}
	secondSkill, _ := os.ReadFile(skillPath)
	secondIndex, _ := os.ReadFile(indexPath)
	if string(firstSkill) != string(secondSkill) || string(firstIndex) != string(secondIndex) {
		t.Fatal("generated skill output changed across identical inputs")
	}

	parts := strings.SplitN(string(firstSkill), "---", 3)
	if len(parts) != 3 {
		t.Fatalf("SKILL.md lacks frontmatter:\n%s", firstSkill)
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &meta); err != nil {
		t.Fatalf("parse SKILL.md frontmatter: %v", err)
	}
	if meta.Name != "fixtureguard" || meta.Description == "" {
		t.Errorf("frontmatter = %+v", meta)
	}
	if strings.Contains(string(firstSkill), "get-caller-identity") || strings.Contains(string(firstSkill), "repoGet") {
		t.Errorf("SKILL.md copied exhaustive command detail:\n%s", firstSkill)
	}

	var index struct {
		Commands []struct {
			Path []string `yaml:"path"`
		} `yaml:"commands"`
	}
	if err := yaml.Unmarshal(firstIndex, &index); err != nil {
		t.Fatalf("parse commands.yaml: %v", err)
	}
	paths := map[string]bool{}
	for _, entry := range index.Commands {
		paths[strings.Join(entry.Path, " ")] = true
	}
	for _, want := range []string{
		"fixtureguard ops aws sts get-caller-identity",
		"fixtureguard ops aws s3 ls",
		"fixtureguard ops forgejo describe",
		"fixtureguard ops forgejo repos read",
		"fixtureguard ops forgejo repos create",
	} {
		if !paths[want] {
			t.Errorf("commands.yaml missing reachable command %q; got %v", want, paths)
		}
	}
}

func TestDiffSpecsDetectsOperationDrift(t *testing.T) {
	committed := []byte(`{
		"paths": { "/repos": {"get": {}}, "/orgs": {"get": {}} },
		"definitions": { "Repo": {"type": "object"} }
	}`)
	live := []byte(`{
		"paths": { "/repos": {"get": {}, "post": {}}, "/teams": {"get": {}} },
		"definitions": { "Repo": {"type": "object"} }
	}`)
	drift, err := diffSpecs(committed, live)
	if err != nil {
		t.Fatalf("diffSpecs: %v", err)
	}
	got := strings.Join(drift, "\n")
	for _, want := range []string{"paths: + /teams", "paths: - /orgs", "paths: ~ /repos"} {
		if !strings.Contains(got, want) {
			t.Errorf("drift missing %q; got:\n%s", want, got)
		}
	}
}

func TestDiffSpecsIgnoresKeyReordering(t *testing.T) {
	committed := []byte(`{"paths": {"/a": {"get": {}, "post": {}}}}`)
	live := []byte(`{"paths": {"/a": {"post": {}, "get": {}}}}`)
	drift, err := diffSpecs(committed, live)
	if err != nil {
		t.Fatalf("diffSpecs: %v", err)
	}
	if len(drift) != 0 {
		t.Errorf("expected no drift on reordering, got %v", drift)
	}
}

func TestGenWritesToExplicitOut(t *testing.T) {
	dir := t.TempDir()
	gfPath := filepath.Join(dir, "forgejo.guardfile.kdl")
	if err := os.WriteFile(gfPath, []byte(guardfileFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "main.go")
	if err := Gen(Options{GuardfilePath: gfPath, Out: out}); err != nil {
		t.Fatalf("Gen: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read generated main.go: %v", err)
	}
	if !strings.Contains(string(b), `Name: "ward-kdl"`) {
		t.Errorf("generated main.go missing the binary name")
	}
}

func TestVerbsErrorWithoutGuardfile(t *testing.T) {
	if err := Gen(Options{}); err == nil {
		t.Error("Gen with no guardfile should error")
	}
	if err := Skew(Options{GuardfilePath: filepath.Join(t.TempDir(), "missing.kdl")}); err == nil {
		t.Error("Skew with missing guardfile should error")
	}
	if err := Build(Options{GuardfilePath: filepath.Join(t.TempDir(), "missing.kdl")}); err == nil {
		t.Error("Build with missing guardfile should error")
	}
}

func TestBuildRefusesWithoutLocks(t *testing.T) {
	dir := t.TempDir()
	gfPath := filepath.Join(dir, "forgejo.guardfile.kdl")
	if err := os.WriteFile(gfPath, []byte(guardfileFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	// No spec lock / specverb.lock beside the Guardfile: Build must refuse with
	// ErrNoLock rather than attempt a network fetch, exactly like Run.
	err := Build(Options{GuardfilePath: gfPath, Out: filepath.Join(dir, "bin")})
	if !errors.Is(err, ErrNoLock) {
		t.Fatalf("Build without locks: want ErrNoLock, got %v", err)
	}
}

func TestBuildRunsEmbeddedFileAndCleansRuntimePath(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 unavailable: %v", err)
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot = filepath.Clean(filepath.Join(repoRoot, "..", ".."))

	dir := t.TempDir()
	writeMember(t, dir, "scripts/report_path.py", "print(__file__)\n")
	gfPath := writeMember(t, dir, "embed.guardfile.kdl", `wrap embed-guard ops measure {
		exec python3
		can run storage {
			argv "-I"
			embed "scripts/report_path.py"
			sealed
		}
	}`)
	if err := Lock(Options{GuardfilePath: gfPath, CLIGuardReplace: repoRoot}); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	bin := filepath.Join(dir, "embed-guard")
	if err := Build(Options{GuardfilePath: gfPath, Out: bin}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	cmd := exec.Command(bin, "ops", "measure", "storage")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run embedded command: %v\n%s", err, out)
	}
	runtimePath := strings.TrimSpace(string(out))
	if !filepath.IsAbs(runtimePath) {
		t.Fatalf("embedded command received non-absolute path %q", runtimePath)
	}
	if _, err := os.Stat(runtimePath); !os.IsNotExist(err) {
		t.Errorf("embedded runtime file remains after command exit: %v", err)
	}
}

func TestResolveBuildDest(t *testing.T) {
	for _, tc := range []struct {
		goos   string
		suffix string
	}{
		{goos: "linux"},
		{goos: "windows", suffix: ".exe"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := resolveBuildDestForOS(tc.goos, "", "forgejo-guardrail"); err == nil {
				t.Error("empty out should error")
			}
			// Existing directory -> binary name joined on.
			got, err := resolveBuildDestForOS(tc.goos, dir, "forgejo-guardrail")
			if err != nil {
				t.Fatalf("dir dest: %v", err)
			}
			if want := filepath.Join(dir, "forgejo-guardrail"+tc.suffix); got != want {
				t.Errorf("dir dest: got %q want %q", got, want)
			}
			// Trailing separator on a not-yet-existing dir -> treated as a directory.
			sub := filepath.Join(dir, "out") + string(os.PathSeparator)
			got, err = resolveBuildDestForOS(tc.goos, sub, "forgejo-guardrail")
			if err != nil {
				t.Fatalf("trailing-sep dest: %v", err)
			}
			if want := filepath.Join(dir, "out", "forgejo-guardrail"+tc.suffix); got != want {
				t.Errorf("trailing-sep dest: got %q want %q", got, want)
			}
			// Explicit file path -> platform-normalized, parent created.
			file := filepath.Join(dir, "nested", "mybin")
			got, err = resolveBuildDestForOS(tc.goos, file, "forgejo-guardrail")
			if err != nil {
				t.Fatalf("file dest: %v", err)
			}
			if want := file + tc.suffix; got != want {
				t.Errorf("file dest: got %q want %q", got, want)
			}
			if _, err := os.Stat(filepath.Dir(file)); err != nil {
				t.Errorf("parent dir not created: %v", err)
			}
		})
	}
}

func TestExecutablePathForOSDoesNotDuplicateEXE(t *testing.T) {
	for _, path := range []string{"aguard.exe", "aguard.EXE"} {
		if got := executablePathForOS("windows", path); got != path {
			t.Errorf("executablePathForOS(windows, %q) = %q", path, got)
		}
	}
}

func TestCopyExecutable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("#!/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "dest")
	if err := copyExecutable(src, dest); err != nil {
		t.Fatalf("copyExecutable: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("dest is not executable: mode %v", info.Mode())
	}
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != "#!/bin/true\n" {
		t.Errorf("dest content mismatch: %q (err %v)", b, err)
	}
}

func TestDiscover_NamesTheRenameRatherThanReportingNoProject(t *testing.T) {
	// A project authored before the rename still has .specgen/ on disk. Saying
	// "no project" would read as a broken checkout rather than a renamed tool.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".specgen"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	_, _, _, err = discover(Options{})
	if err == nil {
		t.Fatal("discover accepted a .specgen/ project with no .umbra/")
	}
	if !strings.Contains(err.Error(), ".specgen") || !strings.Contains(err.Error(), ".umbra") {
		t.Errorf("error = %q, want both directory names so the fix is obvious", err)
	}
}

const yamlSpecMember = `wrap:
  command: [ward-kdl, ops, cloud]
  spec: forgejo.swagger.v1.json
  base_url: forgejo.example/api/v1
  auth:
    scheme: header-token
    header: Authorization
    prefix: "token "
    value: {ssm: /forgejo/api-token}
  can:
    - {verb: read, resource: repos, op: repoGet}
`

const tomlExecMember = `[wrap]
command = ["ward-kdl", "ops", "aws"]
kdl = "exec aws"

[[wrap.can]]
verb = "run"
resource = "sts"
qualifiers = ["get-caller-identity"]

[[wrap.can]]
verb = "run"
resource = "s3"
qualifiers = ["ls"]
kdl = 'deny-when arg0 matches "*tfstate*"'
`

func TestProjectRootDiscoversYAMLAndTOMLMembersBesideKDL(t *testing.T) {
	dir := t.TempDir()
	writeMember(t, dir, "forge/writes.kdl", guardfileFixture)
	writeMember(t, dir, "cloud/reads.yaml", yamlSpecMember)
	selected := writeMember(t, dir, "tools/aws.toml", tomlExecMember)
	// Config that merely lives in the tree is unrelated and never an error.
	writeMember(t, dir, "compose.yaml", "services:\n  web:\n    image: x\n")
	writeMember(t, dir, "Cargo.toml", "[package]\nname = \"x\"\n")
	writeMember(t, dir, "flag.yml", "wrap: true\n")
	g, err := loadGroup(Options{ProjectRoot: dir, GuardfilePath: selected})
	if err != nil {
		t.Fatalf("loadGroup: %v", err)
	}
	var paths, embeds []string
	for _, m := range g.Members {
		paths = append(paths, m.Path)
		embeds = append(embeds, m.Params.GuardfileName)
		if _, err := kdl.ParseString(string(m.Bytes)); err != nil {
			t.Errorf("%s: embedded bytes are not KDL: %v", m.Path, err)
		}
	}
	if got, want := strings.Join(paths, ","), "cloud/reads.yaml,forge/writes.kdl,tools/aws.toml"; got != want {
		t.Errorf("members = %s, want %s", got, want)
	}
	if got, want := strings.Join(embeds, ","), "cloud/reads.yaml.kdl,forge/writes.kdl,tools/aws.toml.kdl"; got != want {
		t.Errorf("embed names = %s, want %s", got, want)
	}
	main, err := g.render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"//go:embed cloud/reads.yaml.kdl", "//go:embed tools/aws.toml.kdl"} {
		if !strings.Contains(string(main), want) {
			t.Errorf("rendered source missing %q", want)
		}
	}
}

func TestProjectRootFailsOnAMalformedYAMLMember(t *testing.T) {
	dir := t.TempDir()
	writeMember(t, dir, "forge/writes.kdl", guardfileFixture)
	writeMember(t, dir, "cloud/typo.yaml", "wrap:\n  command: [ward-kdl, ops, cloud]\n  spce: forgejo.swagger.v1.json\n")
	if _, err := loadGroup(Options{ProjectRoot: dir}); err == nil || !strings.Contains(err.Error(), `unknown key "spce"`) {
		t.Fatalf("a misspelt key in a YAML guardfile must fail the load, got %v", err)
	}
}

func TestLegacyDiscoveryFindsYAMLAndTOMLGuardfiles(t *testing.T) {
	dir := t.TempDir()
	writeMember(t, dir, "a.guardfile.kdl", guardfileFixture)
	writeMember(t, dir, "b.guardfile.yaml", yamlSpecMember)
	writeMember(t, dir, "c.guardfile.toml", tomlExecMember)
	members, err := legacyMembers(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range members {
		got = append(got, m.Path)
	}
	if want := "a.guardfile.kdl,b.guardfile.yaml,c.guardfile.toml"; strings.Join(got, ",") != want {
		t.Errorf("legacy members = %v, want %s", got, want)
	}
}
