package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readEnv(t *testing.T, root, comp string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "env", comp+".env"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestEnvSetUnset(t *testing.T) {
	root := t.TempDir()

	if err := EnvSetLocal(root, "web", []string{"A=1", "B=two words"}); err != nil {
		t.Fatal(err)
	}
	got := readEnv(t, root, "web")
	if got != "A=1\nB=two words\n" {
		t.Fatalf("initial set: %q", got)
	}

	// Update one, add one; comments and unrelated lines survive.
	path := filepath.Join(root, "env", "web.env")
	os.WriteFile(path, []byte("# secrets\nA=1\nB=two words\n"), 0o600)
	if err := EnvSetLocal(root, "web", []string{"A=9", "C=x=y"}); err != nil {
		t.Fatal(err)
	}
	got = readEnv(t, root, "web")
	if !strings.Contains(got, "# secrets") || !strings.Contains(got, "A=9") ||
		!strings.Contains(got, "B=two words") || !strings.Contains(got, "C=x=y") {
		t.Fatalf("after update: %q", got)
	}
	if strings.Contains(got, "A=1") {
		t.Fatalf("old value survived: %q", got)
	}

	if err := EnvUnsetLocal(root, "web", []string{"B"}); err != nil {
		t.Fatal(err)
	}
	got = readEnv(t, root, "web")
	if strings.Contains(got, "B=") || !strings.Contains(got, "A=9") {
		t.Fatalf("after unset: %q", got)
	}

	if err := EnvSetLocal(root, "web", []string{"BAD KEY=1"}); err == nil {
		t.Error("key with space should be rejected")
	}
	if err := EnvSetLocal(root, "web", []string{"NOVALUE"}); err == nil {
		t.Error("missing = should be rejected")
	}
}
