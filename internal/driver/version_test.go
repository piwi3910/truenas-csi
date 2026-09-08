package driver

import (
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// TestVersionLdflagsPathMatchesTheModule fails if the -X symbol path in the
// Makefile or Dockerfile stops naming this module.
//
// This exists because it was wrong, and wrongness here is SILENT: `go build`
// accepts an -X flag naming a symbol that does not exist and simply does
// nothing, so the build succeeds, the release ships, and the binary reports
// "dev" forever. Both files carried github.com/pwatteel/... while the module is
// github.com/piwi3910/..., so no release binary would ever have known its own
// version. Nothing else in the build catches this — only a comparison like this
// one does.
func TestVersionLdflagsPathMatchesTheModule(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("no build info")
	}
	module := info.Main.Path
	if module == "" {
		t.Skip("no main module path available in this build")
	}
	want := module + "/internal/driver.Version"

	// -X may be quoted, and may sit among other ldflags.
	xflag := regexp.MustCompile(`-X\s+([^\s"']+\.Version)`)
	for _, file := range []string{"../../Makefile", "../../Dockerfile"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		found := xflag.FindAllStringSubmatch(string(b), -1)
		if len(found) == 0 {
			t.Errorf("%s sets no -X ...Version ldflag, so the built binary cannot know its version", file)
			continue
		}
		for _, m := range found {
			got := strings.TrimSuffix(m[1], "=")
			if got != want {
				t.Errorf("%s injects the version into %q, but this module is %q.\n"+
					"go build does NOT fail on an -X naming a symbol that does not exist; it "+
					"silently does nothing, so every release binary would report %q.",
					file, got, want, Version)
			}
		}
	}
}
