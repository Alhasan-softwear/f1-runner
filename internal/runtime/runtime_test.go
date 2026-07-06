package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "web.env")
	content := "# comment\n\nAPI_KEY=abc123\nexport PORT=8080\nQUOTED=\"hello world\"\nSINGLE='x=y'\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	env, found, err := LoadEnvFile(path)
	if err != nil || !found {
		t.Fatalf("err=%v found=%v", err, found)
	}
	want := []string{"API_KEY=abc123", "PORT=8080", "QUOTED=hello world", "SINGLE=x=y"}
	if len(env) != len(want) {
		t.Fatalf("got %v, want %v", env, want)
	}
	for i := range want {
		if env[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, env[i], want[i])
		}
	}

	if _, found, err := LoadEnvFile(filepath.Join(dir, "missing.env")); err != nil || found {
		t.Errorf("missing file: err=%v found=%v, want nil/false", err, found)
	}

	bad := filepath.Join(dir, "bad.env")
	os.WriteFile(bad, []byte("NOEQUALS\n"), 0o644)
	if _, _, err := LoadEnvFile(bad); err == nil {
		t.Error("malformed line should error")
	}
}
