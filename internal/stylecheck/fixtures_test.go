package stylecheck

import (
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	wantPattern = regexp.MustCompile(`(?m)//\s*want\s+(.+)$`)
	rulePattern = regexp.MustCompile(`"([A-Z0-9]+)"`)
)

func TestStyleFixtures(t *testing.T) {
	checkFixtureDir(t, "testdata/bad", checkBadFixtureFile)
	checkFixtureDir(t, "testdata/bad_import", checkBadFixtureFile)
	checkFixtureDir(t, "testdata/good", checkGoodFixtureFile)
}

func TestParamCountIsWarning(t *testing.T) {
	src := []byte("package p\nfunc f(a, b, c, d, e int) {}\n")
	diags := analyzeFile(token.NewFileSet(), "p.go", src)
	if len(diags) != 1 || diags[0].rule != "BCS004" || diags[0].severity != severityWarning {
		t.Fatalf("expected single BCS004 warning, got %+v", diags)
	}
}

func TestE2ESleepRule(t *testing.T) {
	src := []byte("package e2e\nimport \"time\"\nfunc f() { time.Sleep(time.Second) }\n")
	diags := analyzeFile(token.NewFileSet(), "test/e2e/foo_test.go", src)
	if len(diags) != 1 || diags[0].rule != "E2E_SLEEP" || diags[0].severity != severityViolation {
		t.Fatalf("expected E2E_SLEEP violation, got %+v", diags)
	}
	prod := analyzeFile(token.NewFileSet(), "internal/node/node.go", src)
	if len(prod) != 0 {
		t.Fatalf("unexpected diagnostics outside test/e2e: %+v", prod)
	}
}

func checkFixtureDir(t *testing.T, dir string, check func(*testing.T, string)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		check(t, filepath.Join(dir, entry.Name()))
	}
}

func readStrippedFixture(t *testing.T, path string) []byte {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stripped := wantPattern.ReplaceAll(src, nil)
	return stripped
}

func checkGoodFixtureFile(t *testing.T, path string) {
	t.Helper()
	diags := analyzeFile(token.NewFileSet(), path, readStrippedFixture(t, path))
	for _, d := range diags {
		t.Errorf("unexpected %s at %s: %s %s", d.severity, d.pos, d.rule, d.message)
	}
}

func checkBadFixtureFile(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	diags := analyzeFile(token.NewFileSet(), path, readStrippedFixture(t, path))
	expected := parseWants(raw)
	for _, d := range diags {
		if d.severity != severityViolation {
			continue
		}
		consumeWant(t, path, expected, d)
	}
	for line, rules := range expected {
		t.Errorf("%s:%d: missing expected violation %v", path, line, rules)
	}
}

func consumeWant(t *testing.T, path string, expected map[int][]string, d diagnostic) {
	t.Helper()
	rules := expected[d.pos.Line]
	if !slices.Contains(rules, d.rule) {
		t.Errorf("unexpected violation at %s:%d: %s %s", path, d.pos.Line, d.rule, d.message)
		return
	}
	remaining := slices.DeleteFunc(rules, func(r string) bool { return r == d.rule })
	if len(remaining) == 0 {
		delete(expected, d.pos.Line)
		return
	}
	expected[d.pos.Line] = remaining
}

func parseWants(src []byte) map[int][]string {
	wants := map[int][]string{}
	for i, line := range strings.Split(string(src), "\n") {
		match := wantPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		for _, quoted := range rulePattern.FindAllStringSubmatch(match[1], -1) {
			wants[i+1] = append(wants[i+1], quoted[1])
		}
	}
	return wants
}
