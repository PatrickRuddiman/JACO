package logging_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoStdlibLogInProduction is the issue #38 lint: production code must use
// the slog-based logging package, never the stdlib "log" package (which would
// reach for log.Default() / log.Printf and bypass the structured pipeline).
//
// It walks every non-test .go file under the repo's cmd/ and internal/ trees
// and fails if any imports "log". The structured pipeline is log/slog only;
// the two binaries build their root logger via logging.New*.
//
// Adding a new subsystem that reaches for the stdlib log package (and thus
// log.Default()) trips this test — exactly the guardrail the acceptance
// criteria asks for.
func TestNoStdlibLogInProduction(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	var offenders []string
	for _, dir := range []string{"cmd", "internal"} {
		base := filepath.Join(root, dir)
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if perr != nil {
				return perr
			}
			for _, imp := range f.Imports {
				if imp.Path.Value == `"log"` {
					rel, _ := filepath.Rel(root, path)
					offenders = append(offenders, rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("production files import the stdlib \"log\" package (use internal/logging + log/slog instead): %s",
			strings.Join(offenders, ", "))
	}
}

// repoRoot walks up from the test's working directory to the module root
// (the dir containing go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}
