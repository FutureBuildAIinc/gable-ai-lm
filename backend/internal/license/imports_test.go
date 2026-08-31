// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package license

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// allowedImports is the complete set of packages the licence seam may import.
//
// This is an ALLOWLIST rather than a blocklist on purpose. Blocking "net/http"
// alone stops the obvious version of the mistake and misses every other one: a
// helper that dials a socket, a client library pulled in for "just telemetry",
// a dependency that phones home three levels down. An allowlist of stdlib
// packages that cannot themselves reach a network makes an outbound call
// unreachable rather than merely unwritten.
//
// That guarantee holds only while every entry is stdlib. This file parses THIS
// package's imports, not their transitive ones, so admitting an internal or
// third-party package would silence the check for everything beneath it —
// exactly the "phones home three levels down" case above. TestAllowlistIsStdlibOnly
// makes that structural: the allowlist cannot admit a non-stdlib package
// without failing, so the reviewer's only remaining job is the one the comment
// asks for — confirming the stdlib package cannot open a connection.
//
// Adding an entry here is a deliberate act. If you are adding one, satisfy
// yourself first that the package cannot open a connection, and second that the
// seam genuinely needs it.
var allowedImports = map[string]bool{
	"bytes":           true,
	"crypto/ed25519":  true,
	"crypto/sha256":   true,
	"encoding/base64": true,
	"encoding/hex":    true,
	"encoding/json":   true,
	"errors":          true,
	"fmt":             true,
	"os":              true,
	"strings":         true,
	"time":            true,
}

// forbiddenImports are named individually so a failure says WHY, rather than
// only "not on the allowlist". These are the ones somebody would actually
// reach for.
var forbiddenImports = map[string]string{
	"net":          "opens sockets",
	"net/http":     "makes HTTP requests",
	"net/url":      "only useful here for building a request",
	"net/rpc":      "makes remote calls",
	"os/exec":      "can shell out to curl",
	"database/sql": "the seam is offline config, not stored state",
	"log/slog":     "Load returns warnings for cmd/server to log; it does not log for itself",
}

// TestNoNetworkOnAnyPath fails if the licence seam ever acquires the ability to
// make a network call.
//
// Self-hosted AI_LM runs on dealer networks that are not always reachable, and
// an air-gapped install must license normally. Separately: an unsolicited
// phone-home from software somebody else is hosting is a trust problem, not a
// feature — v1 is scrape-only by decision, and the destination of any usage
// feed is a business question that has not been answered.
//
// A licence check that fails when the internet does is a licence check that
// takes a dealer's dispatch board down for a DNS outage. This test is what
// stops that from ever shipping.
func TestNoNetworkOnAnyPath(t *testing.T) {
	imports := packageImports(t)

	for path, files := range imports {
		if why, bad := forbiddenImports[path]; bad {
			t.Errorf("%s imports %q (%s) — the license seam must make no network call on any path; imported by %s",
				"internal/license", path, why, strings.Join(files, ", "))
			continue
		}
		if !allowedImports[path] {
			t.Errorf("%s imports %q, which is not on the allowlist in imports_test.go (imported by %s). "+
				"If it genuinely cannot reach a network and the seam needs it, add it there deliberately.",
				"internal/license", path, strings.Join(files, ", "))
		}
	}

	// A misspelled package path would make the loop above vacuous, so assert
	// the fixture actually saw the code.
	if len(imports) < 5 {
		t.Fatalf("only found %d imports (%v) — the import scan is not reading the package", len(imports), keys(imports))
	}
	for _, must := range []string{"crypto/ed25519", "encoding/json"} {
		if _, ok := imports[must]; !ok {
			t.Errorf("expected the package to import %q; the import scan is looking at the wrong files", must)
		}
	}
}

// packageImports maps import path -> the non-test files importing it. Test
// files are excluded: this test itself needs go/parser, and a test helper is
// not a path the server can take at runtime.
func packageImports(t *testing.T) map[string][]string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	out := map[string][]string{}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import %s in %s: %v", spec.Path.Value, name, err)
			}
			out[path] = append(out[path], name)
		}
	}
	if scanned == 0 {
		t.Fatal("found no non-test Go files to scan")
	}
	return out
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestAllowlistIsStdlibOnly keeps the allowlist's promise honest.
//
// packageImports scans one package. Its conclusion — that no outbound call is
// reachable — is sound only because every allowed import is a stdlib package
// with no network of its own. Admit "github.com/…/internal/foo" and the
// conclusion silently stops following: foo's own imports are never examined.
//
// Stdlib import paths have no dot in their first segment (dots appear in domain
// names). That is the whole test, and it is enough: it forces anyone widening
// the allowlist to a module path to confront this comment first.
func TestAllowlistIsStdlibOnly(t *testing.T) {
	for path := range allowedImports {
		first := path
		if i := strings.Index(path, "/"); i >= 0 {
			first = path[:i]
		}
		if strings.Contains(first, ".") {
			t.Errorf(
				"allowlist admits %q, which is not stdlib — this test only scans "+
					"this package's own imports, so a module path here would leave its "+
					"transitive imports unchecked and the no-network claim unproven",
				path,
			)
		}
	}
}
