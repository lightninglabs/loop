package loopd

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLndConnectionOwnershipGuard is a mechanically enforced invariant
// (running as part of the unit tests in CI) that makes sure the lnd
// connection ownership model cannot silently regress:
//
//   - StartAsSubserver must never again accept the closable
//     *lndclient.GrpcLndServices wrapper, only the non-closable
//     *lndclient.LndServices view.
//   - No code in this package may call .Close() on an lnd connection
//     handle (the services view, the raw client connection or the
//     wrapper), except through the designated owner handle lndOwner,
//     which is only set in standalone mode.
func TestLndConnectionOwnershipGuard(t *testing.T) {
	// allowedCloseReceivers is the exhaustive whitelist of receiver
	// expressions on which .Close() may be called on an lnd connection
	// handle in this package. The lndOwner field is only non-nil when
	// the daemon created the connection itself.
	allowedCloseReceivers := map[string]bool{
		// The daemon's owner handle, only set in standalone mode.
		"d.lndOwner": true,

		// The view command's own connection, created and owned by
		// the command itself.
		"ownedLnd": true,
	}

	// forbiddenReceiverComponents are the components (split on '.')
	// that no .Close() receiver expression may contain, unless the
	// whole expression is whitelisted above.
	forbiddenReceiverComponents := []string{
		"lnd", "lndServices", "lndGrpc", "lndClient", "lndOwner",
		"ClientConn",
	}

	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d os.DirEntry,
		err error) error {

		if err != nil {
			return err
		}

		if d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {

			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		checkFileOwnershipGuard(
			t, fset, path, file, allowedCloseReceivers,
			forbiddenReceiverComponents,
		)

		return nil
	})
	require.NoError(t, err)
}

// checkFileOwnershipGuard inspects a single parsed file for violations of
// the lnd connection ownership invariant.
func checkFileOwnershipGuard(t *testing.T, fset *token.FileSet, path string,
	file *ast.File, allowedCloseReceivers map[string]bool,
	forbiddenReceiverComponents []string) {

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			// The subserver entry point must only accept the
			// non-closable services view.
			if node.Name.Name != "StartAsSubserver" {
				return true
			}

			for _, field := range node.Type.Params.List {
				typeStr := renderNode(fset, field.Type)
				if strings.Contains(
					typeStr, "GrpcLndServices",
				) {

					t.Errorf(
						"%v: StartAsSubserver must "+
							"not accept the closable "+
							"wrapper type %v",
						fset.Position(node.Pos()),
						typeStr,
					)
				}
			}

		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Close" {
				return true
			}

			receiver := renderNode(fset, sel.X)
			if allowedCloseReceivers[receiver] {
				return true
			}

			components := strings.FieldsFunc(
				receiver, func(r rune) bool {
					return r == '.' || r == '(' ||
						r == ')' || r == '&' ||
						r == '*'
				},
			)
			for _, component := range components {
				for _, forbidden := range forbiddenReceiverComponents { //nolint:lll
					if component != forbidden {
						continue
					}

					t.Errorf(
						"%v: %v: forbidden .Close() "+
							"call on lnd connection "+
							"receiver %q, only the "+
							"owner handle lndOwner "+
							"may be closed",
						fset.Position(node.Pos()),
						path, receiver,
					)
				}
			}
		}

		return true
	})
}

// renderNode renders an AST node back to its source string.
func renderNode(fset *token.FileSet, n ast.Node) string {
	var buf bytes.Buffer
	_ = printer.Fprint(&buf, fset, n)

	return buf.String()
}
