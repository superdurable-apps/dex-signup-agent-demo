package signup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// eino/go.mod builds in any clone of the repository: a replace line that names a directory names one inside .local/dex
// (gitignored), where make run-eino and make test-eino link the Dex checkout of EINO_DEX_CHECKOUT, never a directory of
// one machine. A replace with a module version stays allowed.
func TestGoModReplacesOnlyWithTheLinkedDexCheckout(t *testing.T) {
	const goMod = "../../go.mod"
	contents, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatal(err)
	}
	link, err := filepath.Abs("../../../.local/dex")
	if err != nil {
		t.Fatal(err)
	}
	module, err := filepath.Abs(filepath.Dir(goMod))
	if err != nil {
		t.Fatal(err)
	}
	directories, block := 0, false
	for _, line := range strings.Split(string(contents), "\n") {
		if comment := strings.Index(line, "//"); comment >= 0 {
			line = line[:comment]
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "replace (":
			block = true
			continue
		case block && line == ")":
			block = false
			continue
		case strings.HasPrefix(line, "replace "):
			line = strings.TrimPrefix(line, "replace ")
		case !block:
			continue
		}
		_, target, found := strings.Cut(line, "=>")
		fields := strings.Fields(target)
		if !found || len(fields) == 0 || len(fields) > 2 {
			t.Fatalf("go.mod has a replace this test cannot read: %q", line)
		}
		if len(fields) == 2 {
			continue // a module path and version, from the module proxy
		}
		directory := fields[0]
		if !strings.HasPrefix(directory, "./") && !strings.HasPrefix(directory, "../") {
			t.Errorf("go.mod replaces with the directory %q: name it relative to eino/, inside ../.local/dex", directory)
			continue
		}
		inside, err := filepath.Rel(link, filepath.Join(module, directory))
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			t.Errorf("go.mod replaces with %q, outside ../.local/dex, the Dex checkout that make links", directory)
			continue
		}
		directories++
	}
	if directories == 0 && !t.Failed() {
		t.Log("go.mod replaces no module with a directory: the released Dex modules need no checkout")
	}
}
