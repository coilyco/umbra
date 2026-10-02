// Package umbra is the no-code driver behind cmd/umbra: the uv-style
// verb surface (gen / lock / skew / run) over a Guardfile. See docs/specverb.md.
package umbra

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	kdl "github.com/calico32/kdl-go"
	"github.com/coilyco/umbra/cli/execverb"
	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/mcpverb"
	"github.com/coilyco/umbra/http/specverb"
	"github.com/coilyco/umbra/http/umbra/codegen"
	"github.com/coilyco/umbra/pkg/flock"
	"github.com/coilyco/umbra/pkg/skillgen"
	"github.com/urfave/cli/v3"
)

// Options are the inputs shared by every driver verb.
type Options struct {
	GuardfilePath   string   // path to the consumer's KDL Guardfile
	ProjectRoot     string   // explicit recursive KDL project boundary (empty discovers .umbra, then keeps legacy discovery)
	BinaryName      string   // gen/build/run: generated CLI/binary name (empty = Guardfile wrap binary)
	Out             string   // gen: main.go output path (debug; cache when empty). build: binary output dir or path
	Args            []string // run: arguments passed through to the materialized binary
	CLIGuardRef     string   // lock: umbra module query to pin (version/commit); empty = auto
	CLIGuardReplace string   // lock: local umbra checkout to replace with (dev locks only)
	Version         string   // build: release version stamped into the binary via -ldflags (empty = "dev")
	SkillsOut       string   // explicit skill root; empty writes no agent-facing artifacts
	ShimDir         string   // install/doctor: the PATH directory an occluded replacement is installed onto
}

// ErrNoLock is returned by run and skew when a required committed lock is
// absent, so the caller can point the user at `umbra lock`.
var ErrNoLock = errors.New("missing committed lock; run 'umbra lock' first")

// ErrSkew is returned by skew when the committed spec lock drifts from upstream.
var ErrSkew = errors.New("spec skew detected")

// member is one guardfile in a merged build. A spec member carries GF (with a
// spec lock + doc); an exec member carries ExecGF (policy only). See driver doc.
type member struct {
	// Path is the slash-normalized path relative to group.Dir. It is the
	// member's stable identity; never use an absolute path in generated inputs.
	Path string
	// SourcePath is the validated on-disk path used only while loading source.
	SourcePath string
	GF         *guardfile.Guardfile // spec dialect; nil for exec and mcp members
	ExecGF     *execverb.Guardfile  // exec dialect; nil for spec and mcp members
	MCPGF      *mcpverb.Guardfile   // mcp dialect; nil for spec and exec members
	Params     codegen.Params
	Bytes      []byte
	Embeds     []embeddedFile
}

// embeddedFile is one validated build input referenced by an exec grant.
type embeddedFile struct {
	Source string
	Name   string
	Bytes  []byte
}

const maxEmbeddedFileBytes = 4 << 20

// isExec reports whether the member speaks the exec dialect.
func (m member) isExec() bool { return m.Params.Transport == codegen.TransportExec }

// isMCP reports whether the member speaks the mcp dialect.
func (m member) isMCP() bool { return m.Params.Transport == codegen.TransportMCP }

// hasLock reports whether the member commits an upstream contract: a pruned
// Swagger document for a spec member, a pruned tool surface for an mcp one.
func (m member) hasLock() bool { return !m.isExec() }

// group is the operation members that compose one merged binary.
type group struct {
	Dir           string
	Binary        string
	RuntimeBinary string
	Members       []member // sorted by Path for a deterministic embed/build order
}

// runtimeBinary is the generated app/file name. It defaults to the source
// Guardfile wrap binary while letting a consumer publish it under another name.
func (g *group) runtimeBinary() string {
	if g.RuntimeBinary != "" {
		return g.RuntimeBinary
	}
	return g.Binary
}

// runtimeExecutable is the platform-specific filesystem name for the generated
// app. The logical urfave command name remains runtimeBinary on every platform.
func (g *group) runtimeExecutable() string {
	return executablePathForOS(runtime.GOOS, g.runtimeBinary())
}

// executablePathForOS gives Windows executables their required .exe suffix
// without changing an explicit suffix or any non-Windows path.
func executablePathForOS(goos, path string) string {
	if goos == "windows" && !strings.EqualFold(filepath.Ext(path), ".exe") {
		return path + ".exe"
	}
	return path
}

// normalizeBinaryName checks a caller-supplied generated binary name before it
// becomes both a cli.Command name and a cached file path.
func normalizeBinaryName(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if strings.TrimSpace(name) != name {
		return "", fmt.Errorf("umbra: --binary must not have leading or trailing whitespace")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("umbra: --binary must be a binary name, not a path")
	}
	return name, nil
}

func newGroup(dir, selector string, members []member, binaryName string) (*group, error) {
	runtimeBinary, err := normalizeBinaryName(binaryName)
	if err != nil {
		return nil, err
	}
	occluded, err := occludedName(members)
	if err != nil {
		return nil, err
	}
	// A replacement's name is not a publishing choice: it is the tool it stands
	// in front of, and installing it under any other name occludes nothing.
	if occluded != "" {
		if runtimeBinary != "" && runtimeBinary != occluded {
			return nil, fmt.Errorf("umbra: --binary %q disagrees with `replace %q`: a replacement is installed under the name it occludes", runtimeBinary, occluded)
		}
		runtimeBinary = occluded
	}
	if runtimeBinary == "" {
		runtimeBinary = selector
	}
	return &group{Dir: dir, Binary: selector, RuntimeBinary: runtimeBinary, Members: members}, nil
}

// occludedName returns the name a replacement member is installed under, empty
// when none declares `replace`. See docs/execverb-replacement.md.
func occludedName(members []member) (string, error) {
	var name string
	for _, m := range members {
		if !m.Params.Replace {
			continue
		}
		if len(members) > 1 {
			return "", fmt.Errorf("umbra: member %q declares `replace` but %d members merge into this binary: a replacement stands in for one tool and takes the binary alone", m.Path, len(members))
		}
		name = m.Params.Occluded
	}
	return name, nil
}

// sniffTransport reads a guardfile's dialect from a child of the `wrap` block:
// `exec` is exec, `mcp` is mcp, otherwise spec. A command path is not a child.
func sniffTransport(src []byte) (string, error) {
	doc, err := kdl.ParseString(string(src))
	if err != nil {
		return "", fmt.Errorf("umbra: parse KDL: %w", err)
	}
	wrap := doc.GetNode("wrap")
	if wrap == nil {
		return "", fmt.Errorf("umbra: missing top-level `wrap` node")
	}
	for _, n := range wrap.Children().Nodes {
		switch n.Name() {
		case "exec":
			return codegen.TransportExec, nil
		case mcpverb.TransportNode:
			return codegen.TransportMCP, nil
		}
	}
	return codegen.TransportSpec, nil
}

// readMember reads a single guardfile, sniffs its transport, and parses+plans
// it with the matching dialect.
func readMember(path, identity string) (member, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied policy input
	if err != nil {
		return member{}, fmt.Errorf("umbra: read guardfile: %w", err)
	}
	b, err := guardfile.Lower(path, raw)
	if err != nil {
		return member{}, fmt.Errorf("umbra: %w", err)
	}
	transport, err := sniffTransport(b)
	if err != nil {
		return member{}, fmt.Errorf("umbra: sniff %s: %w", path, err)
	}
	switch transport {
	case codegen.TransportMCP:
		return readMCPMember(path, identity, b)
	case codegen.TransportExec:
		return readExecMember(path, identity, b)
	default:
		return readSpecMember(path, identity)
	}
}

// readMCPMember parses an mcp-dialect guardfile and plans its member.
func readMCPMember(path, identity string, src []byte) (member, error) {
	gf, err := mcpverb.Parse(src)
	if err != nil {
		return member{}, fmt.Errorf("umbra: parse mcp guardfile %s: %w", path, err)
	}
	p, err := codegen.PlanMCP(gf.Group, gf.Providers(), embeddedGuardfileName(identity), gf.ProviderDecls)
	if err != nil {
		return member{}, err
	}
	// The lock stays beside its member's root-relative identity, so two members
	// with the same wrap name in separate folders never collide.
	p.SpecLockName = filepath.ToSlash(filepath.Join(filepath.Dir(identity), p.SpecLockName))
	return member{Path: identity, SourcePath: path, MCPGF: gf, Params: p, Bytes: src}, nil
}

// readExecMember parses an exec-dialect guardfile and plans its member.
func readExecMember(path, identity string, src []byte) (member, error) {
	egf, err := execverb.Parse(src)
	if err != nil {
		return member{}, fmt.Errorf("umbra: parse exec guardfile %s: %w", path, err)
	}
	p, err := codegen.PlanExec(egf.Group, egf.Providers(), embeddedGuardfileName(identity), egf.ProviderDecls)
	if err != nil {
		return member{}, err
	}
	p.Replace, p.Occluded = egf.Replace, egf.OccludedName()
	embeds, err := readEmbeddedFiles(path, identity, egf.EmbedPaths())
	if err != nil {
		return member{}, err
	}
	for _, embedded := range embeds {
		p.EmbeddedFiles = append(p.EmbeddedFiles, codegen.EmbeddedFile{Source: embedded.Source, Name: embedded.Name})
	}
	return member{Path: identity, SourcePath: path, ExecGF: egf, Params: p, Bytes: src, Embeds: embeds}, nil
}

// readSpecMember parses a spec-dialect guardfile and plans its member.
func readSpecMember(path, identity string) (member, error) {
	// Resolve `inherit` into one self-contained document before the typed parse,
	// so every downstream stage sees the merged grant set (docs/specverb-policy.md).
	flat, err := guardfile.Flatten(path)
	if err != nil {
		return member{}, fmt.Errorf("umbra: resolve guardfile %s: %w", path, err)
	}
	gf, err := guardfile.Parse(flat)
	if err != nil {
		return member{}, fmt.Errorf("umbra: parse guardfile %s: %w", path, err)
	}
	p, err := codegen.Plan(gf, embeddedGuardfileName(identity))
	if err != nil {
		return member{}, err
	}
	// A lock stays beside its member's root-relative source identity. This keeps
	// identically named members in separate folders from sharing an artifact.
	p.SpecLockName = filepath.ToSlash(filepath.Join(filepath.Dir(identity), p.SpecLockName))
	return member{Path: identity, SourcePath: path, GF: gf, Params: p, Bytes: flat}, nil
}

// readEmbeddedFiles resolves each source within the declaring guardfile's
// directory and records its project-relative embed identity.
func readEmbeddedFiles(guardfilePath, identity string, sources []string) ([]embeddedFile, error) {
	base, err := filepath.EvalSymlinks(filepath.Dir(guardfilePath))
	if err != nil {
		return nil, fmt.Errorf("umbra: resolve embedded-file base: %w", err)
	}
	var out []embeddedFile
	for _, source := range sources {
		candidate := filepath.Join(base, filepath.FromSlash(source))
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return nil, fmt.Errorf("umbra: resolve embedded file %s: %w", source, err)
		}
		rel, err := filepath.Rel(base, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("umbra: embedded file %s escapes guardfile directory %s", source, filepath.Dir(guardfilePath))
		}
		info, err := os.Stat(resolved) //nolint:gosec // EvalSymlinks result is confined to the guardfile directory above
		if err != nil {
			return nil, fmt.Errorf("umbra: stat embedded file %s: %w", source, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("umbra: embedded file %s is not a regular file", source)
		}
		if info.Size() > maxEmbeddedFileBytes {
			return nil, fmt.Errorf("umbra: embedded file %s is %d bytes, above the %d-byte limit", source, info.Size(), maxEmbeddedFileBytes)
		}
		data, err := os.ReadFile(resolved) //nolint:gosec // validated build-time source confined beside the guardfile
		if err != nil {
			return nil, fmt.Errorf("umbra: read embedded file %s: %w", source, err)
		}
		name := filepath.ToSlash(filepath.Join(filepath.Dir(identity), filepath.FromSlash(source)))
		out = append(out, embeddedFile{Source: source, Name: name, Bytes: data})
	}
	return out, nil
}

// embeddedGuardfileName names the embedded artifact. A YAML or TOML member
// embeds its lowered KDL, so the name says which format the bytes are in.
func embeddedGuardfileName(identity string) string {
	if guardfile.IsFormatExtension(identity) {
		return identity + ".kdl"
	}
	return identity
}

// operationIntent reports whether malformed KDL is clearly an operation member.
// Such source fails rather than being silently skipped as unrelated project KDL.
func operationIntent(src []byte) bool {
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if line == "wrap" || strings.HasPrefix(line, "wrap ") || strings.HasPrefix(line, "wrap{") {
			return true
		}
	}
	return false
}

func projectRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("umbra: resolve project root: %w", err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("umbra: resolve project root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("umbra: stat project root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("umbra: project root %s is not a directory", path)
	}
	return root, nil
}

func memberIdentity(root, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("umbra: resolve member %s: %w", path, err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("umbra: member %s escapes project root %s", path, root)
	}
	return filepath.ToSlash(rel), nil
}

// discoverProjectMembers recursively finds operation KDL in root.
// Parsed KDL without wrap is unrelated; malformed source mentioning wrap fails.
func discoverProjectMembers(root string) ([]member, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("umbra: walk project: %w", err)
		}
		if d.Type()&os.ModeSymlink != 0 {
			if _, err := memberIdentity(root, path); err != nil {
				return err
			}
		}
		if d.IsDir() || filepath.Ext(path) != ".kdl" && !guardfile.IsFormatExtension(path) {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var members []member
	seenSource := map[string]string{}
	for _, path := range paths {
		m, found, err := projectMember(root, path, seenSource)
		if err != nil {
			return nil, err
		}
		if found {
			members = append(members, m)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Path < members[j].Path })
	return members, nil
}

func projectMember(root, path string, seenSource map[string]string) (member, bool, error) {
	identity, err := memberIdentity(root, path)
	if err != nil {
		return member{}, false, err
	}
	if prior, ok := seenSource[identity]; ok {
		return member{}, false, fmt.Errorf("umbra: duplicate logical member %s (%s and %s)", identity, prior, path)
	}
	seenSource[identity] = path
	src, err := os.ReadFile(path) //nolint:gosec // operator-supplied policy input
	if err != nil {
		return member{}, false, fmt.Errorf("umbra: read member %s: %w", identity, err)
	}
	src, err = guardfile.LowerForDiscovery(path, src)
	if errors.Is(err, guardfile.ErrNotGuardfile) {
		return member{}, false, nil
	}
	if err != nil {
		return member{}, false, fmt.Errorf("umbra: %w", err)
	}
	doc, err := kdl.ParseString(string(src))
	if err != nil {
		if operationIntent(src) {
			return member{}, false, fmt.Errorf("umbra: parse intended operation member %s: %w", identity, err)
		}
		return member{}, false, nil
	}
	if doc.GetNode("wrap") == nil {
		return member{}, false, nil
	}
	m, err := readMember(path, identity)
	return m, err == nil, err
}

func legacyMembers(dir, selected string) ([]member, error) {
	var matches []string
	for _, ext := range []string{"kdl", "yaml", "yml", "toml"} {
		found, err := filepath.Glob(filepath.Join(dir, "*.guardfile."+ext))
		if err != nil {
			return nil, fmt.Errorf("umbra: discover guardfiles: %w", err)
		}
		matches = append(matches, found...)
	}
	if selected != "" {
		found := false
		for _, p := range matches {
			if p == selected {
				found = true
			}
		}
		if !found {
			matches = append(matches, selected)
		}
	}
	sort.Strings(matches)
	var members []member
	for _, path := range matches {
		m, err := readMember(path, filepath.Base(path))
		if err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, nil
}

func validateArtifacts(members []member) error {
	seenLocks := map[string]string{}
	seenArtifacts := map[string]string{}
	for _, m := range members {
		if prior, ok := seenArtifacts[m.Params.GuardfileName]; ok {
			return fmt.Errorf("umbra: conflicting guardfile artifact %s for %s and %s", m.Params.GuardfileName, prior, m.Path)
		}
		seenArtifacts[m.Params.GuardfileName] = m.Path
		if !m.isExec() {
			if prior, ok := seenLocks[m.Params.SpecLockName]; ok {
				return fmt.Errorf("umbra: conflicting spec lock %s for %s and %s", m.Params.SpecLockName, prior, m.Path)
			}
			if prior, ok := seenArtifacts[m.Params.SpecLockName]; ok {
				return fmt.Errorf("umbra: spec lock artifact %s for %s conflicts with %s", m.Params.SpecLockName, m.Path, prior)
			}
			seenLocks[m.Params.SpecLockName] = m.Path
			seenArtifacts[m.Params.SpecLockName] = m.Path
		}
		for _, embedded := range m.Embeds {
			if prior, ok := seenArtifacts[embedded.Name]; ok {
				return fmt.Errorf("umbra: embedded artifact %s for %s conflicts with %s", embedded.Name, m.Path, prior)
			}
			seenArtifacts[embedded.Name] = m.Path
		}
	}
	return nil
}

// loadGroup discovers the operation members that make up one merged binary.
// --project-root is recursive and content-driven; absent it, legacy discovery remains.
func loadGroup(opts Options) (*group, error) {
	dir, selector, members, err := discover(opts)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		if opts.ProjectRoot != "" {
			return nil, fmt.Errorf("umbra: no operation KDL members in project root %s", dir)
		}
		return nil, errors.New("umbra: no *.guardfile.{kdl,yaml,yml,toml} in cwd (set --guardfile or --project-root)")
	}
	byBinary := map[string][]member{}
	order := []string{}
	for _, mem := range members {
		if _, seen := byBinary[mem.Params.Binary]; !seen {
			order = append(order, mem.Params.Binary)
		}
		byBinary[mem.Params.Binary] = append(byBinary[mem.Params.Binary], mem)
	}
	if selector == "" {
		if len(byBinary) != 1 {
			sort.Strings(order)
			return nil, fmt.Errorf("umbra: %d binaries in %s (%s); pass --guardfile to pick one", len(byBinary), dir, strings.Join(order, ", "))
		}
		selector = order[0]
	}
	members, ok := byBinary[selector]
	if !ok {
		return nil, fmt.Errorf("umbra: no guardfile for binary %q in %s", selector, dir)
	}
	if err := validateArtifacts(members); err != nil {
		return nil, err
	}
	return newGroup(dir, selector, members, opts.BinaryName)
}

func discover(opts Options) (string, string, []member, error) {
	if opts.ProjectRoot == "" && opts.GuardfilePath == "" {
		if _, err := os.Stat(".umbra"); err == nil {
			return discoverRoot(".umbra", "")
		} else if !os.IsNotExist(err) {
			return "", "", nil, fmt.Errorf("umbra: inspect conventional project root .umbra: %w", err)
		}
		if err := refusePriorRoot(); err != nil {
			return "", "", nil, err
		}
	}
	if opts.ProjectRoot == "" {
		return discoverLegacy(opts.GuardfilePath)
	}
	return discoverRoot(opts.ProjectRoot, opts.GuardfilePath)
}

// refusePriorRoot names the rename rather than reporting the project as absent.
// The driver was `specgen` and its root `.specgen/` until this rename.
func refusePriorRoot() error {
	if _, err := os.Stat(".specgen"); err == nil {
		return fmt.Errorf("umbra: found .specgen/ but no .umbra/; specgen is now umbra, so rename the directory (`git mv .specgen .umbra`)")
	}
	return nil
}

func discoverLegacy(selected string) (string, string, []member, error) {
	dir := "."
	selector := ""
	if selected != "" {
		dir = filepath.Dir(selected)
		sel, err := readMember(selected, filepath.Base(selected))
		if err != nil {
			return "", "", nil, err
		}
		selector = sel.Params.Binary
	}
	members, err := legacyMembers(dir, selected)
	return dir, selector, members, err
}

func discoverRoot(root, selected string) (string, string, []member, error) {
	dir, err := projectRoot(root)
	if err != nil {
		return "", "", nil, err
	}
	selector, err := selectedBinary(dir, selected)
	if err != nil {
		return "", "", nil, err
	}
	members, err := discoverProjectMembers(dir)
	return dir, selector, members, err
}

func selectedBinary(root, selected string) (string, error) {
	if selected == "" {
		return "", nil
	}
	identity, err := memberIdentity(root, selected)
	if err != nil {
		return "", err
	}
	sel, err := readMember(selected, identity)
	if err != nil {
		return "", err
	}
	return sel.Params.Binary, nil
}

// render emits the merged main.go from the members' pre-planned params, mixing
// spec and exec mounts onto the one binary.
func (g *group) render() ([]byte, error) {
	sp := codegen.SetParams{Binary: g.runtimeBinary()}
	for _, m := range g.Members {
		sp.Mounts = append(sp.Mounts, m.Params)
		if m.Params.Replace {
			sp.Replace, sp.Occluded = true, m.Params.Occluded
		}
		switch {
		case m.isExec():
			sp.HasExec = true
		case m.isMCP():
			sp.HasMCP = true
		default:
			sp.HasSpec = true
		}
	}
	return codegen.RenderParams(sp)
}

// orderedSpecs returns the spec-member spec bytes in member order, the
// deterministic input to the staleness hash. Exec members contribute nothing.
func orderedSpecs(mems []member, byPath map[string][]byte) [][]byte {
	var out [][]byte
	for _, m := range mems {
		if b, ok := byPath[m.Path]; ok {
			out = append(out, b)
		}
	}
	return out
}

// Gen renders the merged consumer main.go into the cache or --out. Agent skill
// output is opt-in through Options.SkillsOut.
func Gen(opts Options) error {
	g, err := loadGroup(opts)
	if err != nil {
		return err
	}
	main, err := g.render()
	if err != nil {
		return err
	}
	out := opts.Out
	if out == "" {
		dir := cacheDirForGroup(g)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("umbra: create cache dir: %w", err)
		}
		out = filepath.Join(dir, "main.go")
	}
	if err := os.WriteFile(out, main, 0o600); err != nil {
		return fmt.Errorf("umbra: write %s: %w", out, err)
	}
	fmt.Fprintf(os.Stderr, "umbra: wrote %s\n", out)
	return emitSkill(g, opts.SkillsOut)
}

// emitSkill writes one concise native skill plus a lazy command index beneath
// the explicit skill root. Empty output is the default and writes nothing.
func emitSkill(g *group, out string) error {
	if out == "" {
		return nil
	}
	app, err := g.commandTree()
	if err != nil {
		return err
	}
	bundle, err := skillgen.RenderSkill(app.Commands, app.Name)
	if err != nil {
		return fmt.Errorf("umbra: render skill: %w", err)
	}
	skillDir := filepath.Join(out, bundle.Name)
	referencesDir := filepath.Join(skillDir, "references")
	if err := os.MkdirAll(referencesDir, 0o750); err != nil {
		return fmt.Errorf("umbra: create skill output: %w", err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(bundle.Skill), 0o644); err != nil { //nolint:gosec // generated skill is intentionally readable
		return fmt.Errorf("umbra: write skill: %w", err)
	}
	indexPath := filepath.Join(referencesDir, "commands.yaml")
	if err := os.WriteFile(indexPath, []byte(bundle.CommandsYAML), 0o644); err != nil { //nolint:gosec // generated skill index is intentionally readable
		return fmt.Errorf("umbra: write skill command index: %w", err)
	}
	fmt.Fprintf(os.Stderr, "umbra: wrote skill %s\n", skillDir)
	return nil
}

// commandTree reconstructs the same merged urfave tree the generated binary
// mounts, without executing a command or resolving credentials.
func (g *group) commandTree() (*cli.Command, error) {
	app := &cli.Command{Name: g.runtimeBinary(), Usage: "guarded verbs generated by umbra"}
	for _, m := range g.Members {
		if m.isExec() {
			embeddedFiles := map[string]string{}
			for _, embedded := range m.Embeds {
				placeholder, err := filepath.Abs(filepath.Join("embedded", filepath.FromSlash(embedded.Source)))
				if err != nil {
					return nil, fmt.Errorf("umbra: resolve embedded-file placeholder %s: %w", embedded.Source, err)
				}
				embeddedFiles[embedded.Source] = placeholder
			}
			if err := execverb.Mount(app, execverb.Config{Guardfile: m.ExecGF, EmbeddedFiles: embeddedFiles}); err != nil {
				return nil, fmt.Errorf("umbra: build exec skill surface %s: %w", m.Path, err)
			}
			continue
		}
		specBytes, err := readSpecLock(g.Dir, m)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("umbra: no spec lock %s for skill output: %w", m.Params.SpecLockName, ErrNoLock)
			}
			return nil, fmt.Errorf("umbra: read spec lock for skill output: %w", err)
		}
		if err := specverb.Mount(app, specverb.Config{Guardfile: m.GF, Spec: specBytes}); err != nil {
			return nil, fmt.Errorf("umbra: build spec skill surface %s: %w", m.Path, err)
		}
	}
	return app, nil
}

// lockSpecs fetches each spec member's upstream spec, prunes it, writes the
// per-member lock, and returns the pruned bytes by path. Exec members skipped.
func lockSpecs(g *group) (map[string][]byte, error) {
	specs := map[string][]byte{}
	for _, m := range g.Members {
		if !m.hasLock() {
			continue
		}
		if m.isMCP() {
			if err := lockTools(g, m, specs); err != nil {
				return nil, err
			}
			continue
		}
		full, err := loadFullSpec(m)
		if err != nil {
			return nil, fmt.Errorf("umbra: load spec %s: %w", m.Params.GuardfileName, err)
		}
		// Commit only the granted slice, not the full upstream dump: the lock
		// becomes the consumer's own contract. See docs/umbra-cli.md.
		specBytes, err := specverb.Prune(full, m.GF)
		if err != nil {
			return nil, fmt.Errorf("umbra: prune spec %s: %w", m.Params.GuardfileName, err)
		}
		specLockPath := filepath.Join(g.Dir, m.Params.SpecLockName)
		if err := os.MkdirAll(filepath.Dir(specLockPath), 0o750); err != nil {
			return nil, fmt.Errorf("umbra: create spec lock dir: %w", err)
		}
		encodedSize, err := writeSpecLock(specLockPath, specBytes)
		if err != nil {
			return nil, fmt.Errorf("umbra: write spec lock: %w", err)
		}
		specs[m.Path] = specBytes
		fmt.Fprintf(os.Stderr, "umbra: locked %s (%d encoded bytes, %d decoded, pruned from %d)\n", m.Params.SpecLockName, encodedSize, len(specBytes), len(full))
	}
	return specs, nil
}

// lockTools connects to an mcp member's upstream, prunes the tool surface to
// the granted set, and writes the lock through the spec dialect's encoder.
func lockTools(g *group, m member, specs map[string][]byte) error {
	live, err := fetchTools(m)
	if err != nil {
		return fmt.Errorf("umbra: list tools %s: %w", m.Params.GuardfileName, err)
	}
	pruned, err := pruneTools(m.MCPGF, live)
	if err != nil {
		return fmt.Errorf("umbra: prune tools %s: %w", m.Params.GuardfileName, err)
	}
	encoded, err := encodeTools(pruned)
	if err != nil {
		return fmt.Errorf("umbra: %s: %w", m.Params.GuardfileName, err)
	}
	lockPath := filepath.Join(g.Dir, m.Params.SpecLockName)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		return fmt.Errorf("umbra: create tool lock dir: %w", err)
	}
	size, err := writeSpecLock(lockPath, encoded)
	if err != nil {
		return fmt.Errorf("umbra: write tool lock: %w", err)
	}
	specs[m.Path] = encoded
	fmt.Fprintf(os.Stderr, "umbra: locked %s (%d encoded bytes, %d tools of %d upstream)\n",
		m.Params.SpecLockName, size, len(pruned), len(live))
	return nil
}

// skewTools reports drift between an mcp member's committed tool lock and its
// live upstream, pruned to the same granted surface.
func skewTools(g *group, m member) ([]string, error) {
	committedBytes, err := readSpecLock(g.Dir, m)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("umbra: no tool lock %s: %w", m.Params.SpecLockName, ErrNoLock)
		}
		return nil, fmt.Errorf("umbra: read tool lock: %w", err)
	}
	committed, err := decodeTools(committedBytes)
	if err != nil {
		return nil, fmt.Errorf("umbra: %s: %w", m.Params.SpecLockName, err)
	}
	live, err := fetchTools(m)
	if err != nil {
		return nil, fmt.Errorf("umbra: list tools %s: %w", m.Params.GuardfileName, err)
	}
	// Pruning live to the same granted surface keeps drift attributable to what
	// this consumer actually mounts, matching the spec dialect.
	livePruned, err := pruneTools(m.MCPGF, live)
	if err != nil {
		return nil, fmt.Errorf("umbra: prune tools %s: %w", m.Params.GuardfileName, err)
	}
	return diffTools(committed, livePruned)
}

// loadFullSpec returns the member's full upstream spec: a spec vendored beside
// the guardfile is read directly, else fetched from the derived URL.
func loadFullSpec(m member) ([]byte, error) {
	if m.GF != nil && m.GF.Spec != "" {
		local := filepath.Join(filepath.Dir(m.SourcePath), m.GF.Spec)
		b, readErr := readSpecSource(local)
		if readErr == nil {
			fmt.Fprintf(os.Stderr, "umbra: read vendored spec %s\n", m.GF.Spec)
			return b, nil
		}
		if !os.IsNotExist(readErr) {
			return nil, fmt.Errorf("read vendored spec %s: %w", m.GF.Spec, readErr)
		}
	}
	return fetchSpec(m.Params.SpecURL)
}

// Lock refreshes both committed locks: each member's pruned spec lock and the
// one specverb.lock (the frozen build module). Skill output remains opt-in.
func Lock(opts Options) error {
	g, err := loadGroup(opts)
	if err != nil {
		return err
	}
	specs, err := lockSpecs(g)
	if err != nil {
		return err
	}
	// One merged dep lock for the whole binary - the module graph is the union.
	main, err := g.render()
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "specverb-lock-")
	if err != nil {
		return fmt.Errorf("umbra: temp build dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := materializeModuleDir(tmp, main, g.Members, specs); err != nil {
		return err
	}
	dl, err := resolveDepLock(tmp, opts.CLIGuardRef, opts.CLIGuardReplace)
	if err != nil {
		return err
	}
	if err := writeDepLock(filepath.Join(g.Dir, LockName), dl); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "umbra: locked %s (umbra %s)\n", LockName, dl.CLIGuard)
	return emitSkill(g, opts.SkillsOut)
}

// Skew reports drift between each member's committed lock and live upstream,
// never writing. ErrSkew signals drift; a fetch failure is a plain error.
func Skew(opts Options) error {
	g, err := loadGroup(opts)
	if err != nil {
		return err
	}
	var drift []string
	for _, m := range g.Members {
		if !m.hasLock() {
			continue // exec members have no upstream contract to drift against
		}
		var d []string
		if m.isMCP() {
			d, err = skewTools(g, m)
		} else {
			d, err = skewSpec(g, m)
		}
		if err != nil {
			return err
		}
		// Prefix each line with the member so a merged binary's drift is
		// attributable to the upstream that moved.
		for _, line := range d {
			drift = append(drift, m.Params.GuardfileName+": "+line)
		}
	}
	if len(drift) > 0 {
		fmt.Fprintf(os.Stderr, "umbra: %d change(s) since lock:\n", len(drift))
		for _, d := range drift {
			fmt.Fprintf(os.Stderr, "  %s\n", d)
		}
		return ErrSkew
	}
	fmt.Fprintln(os.Stderr, "umbra: no skew; committed locks match upstream")
	return nil
}

// skewSpec diffs one spec member's committed lock against live upstream.
func skewSpec(g *group, m member) ([]string, error) {
	committed, err := readSpecLock(g.Dir, m)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("umbra: no spec lock %s: %w", m.Params.SpecLockName, ErrNoLock)
		}
		return nil, fmt.Errorf("umbra: read spec lock: %w", err)
	}
	live, err := fetchSpec(m.Params.SpecURL)
	if err != nil {
		return nil, fmt.Errorf("umbra: fetch spec %s: %w", m.Params.GuardfileName, err)
	}
	// Prune live to the same granted slice the committed lock holds, so drift is
	// reported only for operations this consumer exposes.
	livePruned, err := specverb.Prune(live, m.GF)
	if err != nil {
		return nil, fmt.Errorf("umbra: prune live spec %s: %w", m.Params.GuardfileName, err)
	}
	return diffSpecs(committed, livePruned)
}

// Run materializes the consumer binary out-of-band (building only when stale)
// and execs it. It refuses to run without committed locks rather than auto-locking.
func Run(opts Options) error {
	binPath, g, err := materialize(opts)
	if err != nil {
		return err
	}
	if err := emitSkill(g, opts.SkillsOut); err != nil {
		return err
	}
	return execBinary(binPath, opts.Args)
}

// Build materializes the consumer binary out-of-band (same cache + staleness
// path as Run) and copies it to opts.Out instead of execing it. See umbra-cli.md.
func Build(opts Options) error {
	binPath, g, err := materialize(opts)
	if err != nil {
		return err
	}
	dest, err := resolveBuildDest(opts.Out, g.runtimeBinary())
	if err != nil {
		return err
	}
	if err := copyExecutable(binPath, dest); err != nil {
		return err
	}
	if err := emitSkill(g, opts.SkillsOut); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "umbra: built %s\n", dest)
	return nil
}

// materialize is the shared prelude of Run and Build: it builds the merged
// binary into the cache when stale and returns its path. Refuses without locks.
func materialize(opts Options) (string, *group, error) {
	g, err := loadGroup(opts)
	if err != nil {
		return "", nil, err
	}
	specByPath := map[string][]byte{}
	for _, m := range g.Members {
		if m.isExec() {
			continue
		}
		specBytes, err := readSpecLock(g.Dir, m)
		if err != nil {
			if os.IsNotExist(err) {
				return "", g, fmt.Errorf("umbra: no spec lock %s: %w", m.Params.SpecLockName, ErrNoLock)
			}
			return "", g, fmt.Errorf("umbra: read spec lock: %w", err)
		}
		specByPath[m.Path] = specBytes
	}
	depLockPath := filepath.Join(g.Dir, LockName)
	depRaw, err := os.ReadFile(depLockPath) //nolint:gosec // committed dep lock
	if err != nil {
		if os.IsNotExist(err) {
			return "", g, fmt.Errorf("umbra: no %s: %w", LockName, ErrNoLock)
		}
		return "", g, fmt.Errorf("umbra: read %s: %w", LockName, err)
	}
	dl, err := readDepLock(depLockPath)
	if err != nil {
		return "", g, err
	}
	cdir := cacheDirForGroup(g)
	main, err := g.render()
	if err != nil {
		return "", g, err
	}
	binPath := filepath.Join(cdir, "bin", g.runtimeExecutable())
	want := stamp{
		GuardfileHash:    hashMembers(g.Members),
		SpecLockHash:     hashConcat(orderedSpecs(g.Members, specByPath)...),
		DepLockHash:      hashBytes(depRaw),
		GeneratorVersion: DriverVersion(),
		LDVersion:        opts.Version,
		BuiltAt:          time.Now().UTC().Format(time.RFC3339),
	}
	if err := materializeIfStale(cdir, binPath, main, g.Members, specByPath, dl, want); err != nil {
		return "", g, err
	}
	return binPath, g, nil
}

// hashMembers combines the raw guardfile bytes of a group (members are
// pre-sorted by path) into one staleness hash.
func hashMembers(mems []member) string {
	bss := make([][]byte, 0, len(mems)*2)
	for _, m := range mems {
		bss = append(bss, []byte(m.Path+"\x00"), m.Bytes)
		for _, embedded := range m.Embeds {
			bss = append(bss, []byte(embedded.Name+"\x00"), embedded.Bytes)
		}
	}
	return hashConcat(bss...)
}

// resolveBuildDest follows go build -o: directories take the binary name, while
// explicit paths stay explicit. Windows adds .exe to either form when absent.
func resolveBuildDest(out, binary string) (string, error) {
	return resolveBuildDestForOS(runtime.GOOS, out, binary)
}

func resolveBuildDestForOS(goos, out, binary string) (string, error) {
	if out == "" {
		return "", fmt.Errorf("umbra: build needs an output path (--out)")
	}
	dest := out
	if strings.HasSuffix(out, string(os.PathSeparator)) {
		dest = filepath.Join(out, binary)
	} else if info, err := os.Stat(out); err == nil && info.IsDir() {
		dest = filepath.Join(out, binary)
	}
	dest = executablePathForOS(goos, dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", fmt.Errorf("umbra: create output dir: %w", err)
	}
	return dest, nil
}

// copyExecutable copies the cached binary to dest via temp file + rename, so an
// older copy running at dest is replaced atomically, not truncated ("text file busy").
func copyExecutable(src, dest string) error {
	in, err := os.Open(src) //nolint:gosec // driver-built cache binary
	if err != nil {
		return fmt.Errorf("umbra: open built binary: %w", err)
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".specverb-build-*")
	if err != nil {
		return fmt.Errorf("umbra: create temp binary: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("umbra: copy binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("umbra: close temp binary: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil { //nolint:gosec // executable output
		return fmt.Errorf("umbra: chmod binary: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("umbra: install binary: %w", err)
	}
	return nil
}

// lockCache serialises materialize+build against cdir. Where the platform has
// no advisory lock it says so and continues. See docs/umbra-materialization.md.
func lockCache(lf *os.File, cdir string) error {
	err := flock.Exclusive(lf)
	if errors.Is(err, flock.ErrUnsupported) {
		fmt.Fprintf(os.Stderr, "umbra: no cache lock on %s, building %s unserialised (a concurrent run may race)\n", runtime.GOOS, cdir)
		return nil
	}
	if err != nil {
		return fmt.Errorf("umbra: lock cache: %w", err)
	}
	return nil
}

// materializeIfStale rebuilds the binary under the cache lock when its inputs
// changed, releasing the lock before return so Run can exec the fresh image.
func materializeIfStale(cdir, binPath string, main []byte, mems []member, specByPath map[string][]byte, dl *DepLock, want stamp) error {
	if err := os.MkdirAll(cdir, 0o750); err != nil {
		return fmt.Errorf("umbra: create cache dir: %w", err)
	}
	lf, err := os.OpenFile(filepath.Join(cdir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // driver-owned cache dir
	if err != nil {
		return fmt.Errorf("umbra: open cache lock: %w", err)
	}
	defer func() { _ = lf.Close() }()
	if err := lockCache(lf, cdir); err != nil {
		return err
	}
	defer func() { _ = flock.Unlock(lf) }()

	if !stale(cdir, binPath, dl, want) {
		return nil
	}
	if err := materializeModuleDir(cdir, main, mems, specByPath); err != nil {
		return err
	}
	if err := writeModuleFiles(cdir, dl); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(cdir, "bin"), 0o750); err != nil {
		return fmt.Errorf("umbra: create bin dir: %w", err)
	}
	args := []string{"build", "-mod=readonly"}
	// Stamp the consumer's release version into main.Version, mirroring the way
	// ward's own formula sets it. Absent (dev build), the template's "dev" stands.
	if want.LDVersion != "" {
		args = append(args, "-ldflags", "-X main.Version="+want.LDVersion)
	}
	args = append(args, "-o", binPath, ".")
	if err := runGo(cdir, args...); err != nil {
		return err
	}
	return writeStamp(cdir, want)
}

// materializeModuleDir writes the build inputs into dir: the rendered main.go
// plus each member's embeds (guardfile always, spec lock for spec members).
func materializeModuleDir(dir string, main []byte, mems []member, specByPath map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("umbra: create module dir: %w", err)
	}
	files := map[string][]byte{"main.go": main}
	for _, m := range mems {
		files[m.Params.GuardfileName] = m.Bytes
		if m.hasLock() {
			files[m.Params.SpecLockName] = specByPath[m.Path]
		}
		for _, embedded := range m.Embeds {
			files[embedded.Name] = embedded.Bytes
		}
	}
	for name, b := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("umbra: create embed dir: %w", err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return fmt.Errorf("umbra: write %s: %w", name, err)
		}
	}
	return nil
}

// fetchSpec GETs the upstream Swagger document, the source for both lock and
// skew.
func fetchSpec(specURL string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(specURL) //nolint:gosec // URL derived from the Guardfile base-url
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s -> %s", specURL, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// diffSpecs reports operation-level drift between two Swagger documents by the
// paths/definitions keys, normalized through JSON to ignore key reordering.
func diffSpecs(committed, live []byte) ([]string, error) {
	c, err := normalizeSpec(committed)
	if err != nil {
		return nil, fmt.Errorf("umbra: parse committed spec lock: %w", err)
	}
	l, err := normalizeSpec(live)
	if err != nil {
		return nil, fmt.Errorf("umbra: parse live spec: %w", err)
	}
	var drift []string
	for _, section := range []string{"paths", "definitions"} {
		drift = append(drift, diffSection(section, mapOf(c[section]), mapOf(l[section]))...)
	}
	sort.Strings(drift)
	return drift, nil
}

// normalizeSpec unmarshals a Swagger document into a generic map for structural
// comparison.
func normalizeSpec(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// mapOf coerces a decoded JSON value to a string-keyed map, or nil.
func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// diffSection compares one section's keys, emitting "+ key" for additions,
// "- key" for removals, and "~ key" for entries whose canonical JSON changed.
func diffSection(section string, committed, live map[string]any) []string {
	var out []string
	for k := range live {
		if _, ok := committed[k]; !ok {
			out = append(out, fmt.Sprintf("%s: + %s", section, k))
		}
	}
	for k, cv := range committed {
		lv, ok := live[k]
		if !ok {
			out = append(out, fmt.Sprintf("%s: - %s", section, k))
			continue
		}
		if canonical(cv) != canonical(lv) {
			out = append(out, fmt.Sprintf("%s: ~ %s", section, k))
		}
	}
	return out
}

// canonical renders v as a stable JSON string (Go marshals map keys sorted), so
// two structurally equal values compare equal regardless of source ordering.
func canonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
