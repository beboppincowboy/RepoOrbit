package archtest

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/repoorbit/repoorbit/backend"

// forbidden lists, per package directory under internal/, the import paths (and everything under them) its non-test
// files must not import.
var forbidden = map[string][]string{
	"issues/application": {
		"net/http",
		module + "/graphql",
		"github.com/99designs/gqlgen",
		module + "/internal/githubapp",
		module + "/internal/db",
		"github.com/jackc/pgx/v5",
		module + "/internal/accessfence",
		module + "/internal/ingestion",
		module + "/internal/repositories",
	},
	"readguard": {
		"net/http",
		module + "/graphql",
		"github.com/99designs/gqlgen",
	},
	"board": {
		"net/http",
		module + "/graphql",
		"github.com/99designs/gqlgen",
	},
}

func imports(t *testing.T, dir string) []string {
	t.Helper()
	pkg, err := build.Default.ImportDir(filepath.Join("..", dir), 0)
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	return pkg.Imports
}

func TestPackageBoundaries(t *testing.T) {
	for _, dir := range []string{"issues/domain"} {
		for _, imp := range imports(t, dir) {
			if strings.Contains(strings.Split(imp, "/")[0], ".") {
				t.Errorf("%s imports non-standard package %s", dir, imp)
			}
		}
	}
	for dir, banned := range forbidden {
		for _, imp := range imports(t, dir) {
			for _, p := range banned {
				if imp == p || strings.HasPrefix(imp, p+"/") {
					t.Errorf("%s imports %s", dir, imp)
				}
			}
		}
	}
}
