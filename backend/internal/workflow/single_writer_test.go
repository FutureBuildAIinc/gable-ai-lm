// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// theOneWriter is the only place in this server that may put a route on the
// dealer's dispatch board.
const theOneWriter = "internal/workflow/service.go"

// TestPushDeliveryRouteHasExactlyOneWriter pins the module's central invariant
// at the level where it is actually decided: how many places in the binary can
// reach the ERP's delivery-route write-back.
//
// Every safety mechanism this package owns — gateSupersede, clearDisplacedClaims,
// the recall paths, the plan state machine — reasons about the board by reading
// the live-route ledger that Push writes alongside each upstream call. A ledger
// entry is not bookkeeping; it is the ONLY evidence those gates have that a
// truck is spoken for. So a second caller of PushDeliveryRoute does not merely
// duplicate work: it puts routes on the board that no ledger names, and every
// gate downstream is blind to them by construction. A later re-ingest plans
// straight over the top and the dealer holds two routes for one truck.
//
// internal/routing used to be exactly that second writer. Its
// `POST /api/v1/routing/plan/{id}/approve` pushed routes upstream and wrote no
// ledger entry of any kind. It was deleted rather than wrapped, because the
// workflow module already does this job with a ledger, gates and recall.
//
// A test is used here instead of review discipline because the failure is
// invisible in the diff that causes it. Adding `sink.PushDeliveryRoute(...)` to
// some other module is three lines that compile, pass that module's own tests,
// and break an invariant enforced nowhere near them. This test is the thing
// that says no.
//
// If you are here because this test failed on a call site you just added: the
// answer is not to add your file to the allowlist. It is to route your write
// through workflow's Push, so it gets a ledger entry and the gates can see it.
func TestPushDeliveryRouteHasExactlyOneWriter(t *testing.T) {
	callers, scanned := pushDeliveryRouteCallers(t)

	// Sanity: a walker that silently read nothing would make every assertion
	// below vacuously true, which is the one way this guard could fail open.
	if scanned < 25 {
		t.Fatalf("scanned only %d non-test .go files — the source walk is not reading the tree", scanned)
	}
	if len(callers) == 0 {
		t.Fatalf("found no PushDeliveryRoute call sites at all in %d files; expected %s — the scan is looking in the wrong place",
			scanned, theOneWriter)
	}

	for _, c := range callers {
		if c != theOneWriter {
			t.Errorf("%s calls PushDeliveryRoute, but %s must be the only writer to the dealer's dispatch board.\n"+
				"A push that does not also write a live-route ledger entry is invisible to gateSupersede, "+
				"clearDisplacedClaims and every recall path, so a later re-ingest will plan over it. "+
				"Route this write through workflow's Push instead of calling the sink directly.",
				c, theOneWriter)
		}
	}
	if len(callers) > 1 {
		t.Errorf("PushDeliveryRoute has %d call sites (%s); exactly one is allowed", len(callers), strings.Join(callers, ", "))
	}
}

// pushDeliveryRouteCallers returns the module-relative paths of every non-test
// file containing a call to PushDeliveryRoute, plus the number of files scanned.
//
// It matches CALLS (an ast.CallExpr on a selector), not mentions. The method's
// own declaration in internal/gable/client.go and the interface method specs
// that declare the seam are therefore not counted — the question is who can
// invoke the write, not who names it. Test files are excluded: the fakes and
// their assertions are not paths the server can take at runtime.
func pushDeliveryRouteCallers(t *testing.T) (paths []string, scanned int) {
	t.Helper()

	// From internal/workflow, the module root is two levels up. Resolve it so a
	// failure prints a path a human can act on.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the Go module root at %s (no go.mod): %v", root, err)
	}

	fset := token.NewFileSet()
	found := map[string]bool{}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name != "." && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		scanned++

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relativize %s: %v", path, err)
		}
		rel = filepath.ToSlash(rel)

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "PushDeliveryRoute" {
				found[rel] = true
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}

	for p := range found {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, scanned
}
