package internal_test

import (
	"os/exec"
	"strings"
	"testing"
)

const root = "ottstreamer/internal/"

// Add a name here when you add a part of the domain.
var domainPackages = []string{"timeline", "stream", "adbreak", "live", "event"}

func TestDomainPackagesImportNoAdapters(t *testing.T) {
	for _, name := range domainPackages {
		out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", root+name).Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, dep := range strings.Fields(string(out)) {
			sub, ok := strings.CutPrefix(dep, root)
			if ok && strings.Contains(sub, "/") {
				t.Errorf("%s depends on %s", name, dep)
			}
		}
	}
}
