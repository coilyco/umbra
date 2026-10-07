package guardfile

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// structuralNodes are the top-level names a parser reads as the subject of the
// file rather than a sibling of it.
var structuralNodes = map[string]bool{"wrap": true, "mcp-upstream": true}

// TestSiblingNodesCoverEveryParseSite fails on any parser `GetNode(<name>)` read
// of a top-level node that SiblingNodes does not list.
func TestSiblingNodesCoverEveryParseSite(t *testing.T) {
	listed := SiblingNodes()
	roots := []string{"../../http", "../../cli/execverb"}
	reads := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 1 {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "GetNode" {
					return true
				}
				name, known := nodeName(call.Args[0])
				switch {
				case !known:
					t.Errorf("%s: GetNode argument is not a literal or a named node constant; list it in siblingNodes or name it through a constant this test knows", path)
				case structuralNodes[name]:
				default:
					reads++
					if !slices.Contains(listed, name) {
						t.Errorf("%s reads top-level %q, which SiblingNodes does not list", path, name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if reads == 0 {
		t.Fatal("found no sibling reads; the walk is no longer reaching the parsers")
	}
}

// nodeName resolves a GetNode argument to the node name it reads: a string
// literal, or one of the named constants the parsers use.
func nodeName(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		return identNode(v.Name)
	case *ast.SelectorExpr:
		return identNode(v.Sel.Name)
	}
	return "", false
}

func identNode(name string) (string, bool) {
	switch name {
	case "DescriptionNode":
		return DescriptionNode, true
	case "UpstreamNode":
		return "mcp-upstream", true
	}
	return "", false
}

// TestSiblingNodesAreRead pins the other direction: every listed name is read
// by the spec-mode parser, which fails closed on a malformed value.
func TestSiblingNodesAreRead(t *testing.T) {
	base := `wrap ward ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; value ssm "/forgejo/api-token" }
    can get repos
}`
	for _, name := range SiblingNodes() {
		_, err := Parse([]byte(name + " \"a\" \"b\"\n" + base))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a malformed %q sibling parsed as %v, so the parser does not read it", name, err)
		}
	}
}
