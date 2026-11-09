package netguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// outboundClientExceptions are the HTTP clients that may use Go's default
// transport, which reads a proxy from the environment, or http.DefaultClient,
// with why. Everything else Gofer sends goes through a netguard transport, so
// it takes the right account's proxy, or the default one.
var outboundClientExceptions = map[string]string{
	"auth/generic_oidc.go":       "signing in to Gofer itself, not mail",
	"googlefonts/googlefonts.go": "Google Fonts for the interface, not mail",
	"translation/google_web.go":  "translation the reader asks for, not mail",
	"server/health.go":           "health check against Gofer itself",
	"mail/jmap/session.go":       "fallback for a client without HTTP; accounts always set it",
}

// A new HTTP client in mail code that leaves out netguard would connect
// directly, or through a proxy from the environment, whatever proxy the
// account chose.
func TestOutboundHTTPClientsUseNetguard(t *testing.T) {
	root := ".."
	fset := token.NewFileSet()
	needed := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "_templ.go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "netguard/") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		httpName := ""
		for _, spec := range file.Imports {
			if spec.Path.Value == `"net/http"` {
				httpName = "http"
				if spec.Name != nil {
					httpName = spec.Name.Name
				}
			}
		}
		if httpName == "" {
			return nil
		}
		isHTTP := func(expr ast.Expr, name string) bool {
			selector, ok := expr.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != name {
				return false
			}
			pkg, ok := selector.X.(*ast.Ident)
			return ok && pkg.Name == httpName
		}
		ast.Inspect(file, func(node ast.Node) bool {
			problem := ""
			switch n := node.(type) {
			case *ast.SelectorExpr:
				if isHTTP(n, "DefaultClient") || isHTTP(n, "DefaultTransport") {
					problem = "uses http." + n.Sel.Name
				}
			case *ast.CompositeLit:
				if isHTTP(n.Type, "Client") {
					hasTransport := false
					for _, element := range n.Elts {
						if kv, ok := element.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Transport" {
								hasTransport = true
							}
						}
					}
					if !hasTransport {
						problem = "makes an http.Client without a Transport"
					}
				}
			}
			if problem != "" {
				needed[rel] = true
				if _, excepted := outboundClientExceptions[rel]; !excepted {
					t.Errorf("%s %s; give it a netguard transport, or add it to outboundClientExceptions with why", fset.Position(node.Pos()), problem)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel := range outboundClientExceptions {
		if !needed[rel] {
			t.Errorf("%s no longer needs its outboundClientExceptions entry; remove it", rel)
		}
	}
}
