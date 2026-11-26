package archtest

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageBoundaries(t *testing.T) {
	for _, dir := range []string{"issues/domain"} {
		pkg, err := build.Default.ImportDir(filepath.Join("..", dir), 0)
		if err != nil {
			t.Fatalf("load %s: %v", dir, err)
		}
		for _, imp := range pkg.Imports {
			if strings.Contains(strings.Split(imp, "/")[0], ".") {
				t.Errorf("%s imports non-standard package %s", dir, imp)
			}
		}
	}
}
