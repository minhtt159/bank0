package config

import (
	"os"
	"regexp"
	"testing"
)

// TestVersionNoDrift cross-checks the release version across its three
// hand-maintained copies. publish.yml's chart job already asserts Chart.yaml
// matches the git tag and fails the release if not, but nothing watched the two
// Go-side copies: during the v1.1.0 prep they were missed and would have shipped
// a v1.1.0 binary reporting "1.0.2" from /health and on the console dashboard
// card. Anchoring both to Chart.yaml chains them to that assertion — tag ==
// appVersion (publish.yml) == config default == config.yaml (here) — so a bump
// that touches only the chart is a red test on the bump PR, not a wrong version
// discovered in production.
func TestVersionNoDrift(t *testing.T) {
	want := grep1(t, "../../deploy/helm/bank0/Chart.yaml", `(?m)^appVersion:\s*"?([0-9]+\.[0-9]+\.[0-9]+[^"\s]*)"?`)

	for _, c := range []struct{ path, pattern string }{
		{"config.go", `v\.SetDefault\("app\.version", "([^"]+)"\)`},
		{"../../config.yaml", `(?m)^\s+version:\s*"?([^"\s]+)"?`},
	} {
		if got := grep1(t, c.path, c.pattern); got != want {
			t.Errorf("%s has version %q but Chart.yaml appVersion is %q", c.path, got, want)
		}
	}
}

// grep1 returns the first capture group of re in path, failing the test if the
// pattern no longer matches — a silently-zero match would make this check pass
// on a file it can no longer read.
func grep1(t *testing.T, path, pattern string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(pattern).FindSubmatch(b)
	if m == nil {
		t.Fatalf("no match for %s in %s — the version moved or was reformatted; fix this test's regex", pattern, path)
	}
	return string(m[1])
}
