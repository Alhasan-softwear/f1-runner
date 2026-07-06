package config

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, yml string) *Root {
	t.Helper()
	r, err := ParseRoot([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const graphRoot = `
project: demo
repo: /srv/repo.git
servers:
  s1: { host: h, user: u }
components:
  db:     { path: apps/db,     servers: [s1] }
  api:    { path: apps/api,    servers: [s1], depends_on: [db] }
  web:    { path: apps/web,    servers: [s1], depends_on: [api] }
  worker: { path: apps/worker, servers: [s1], depends_on: [db] }
`

func TestWavesOrdering(t *testing.T) {
	r := mustParse(t, graphRoot)
	waves, err := r.Waves([]string{"web", "api", "db", "worker"})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(waves))
	for i, w := range waves {
		got[i] = strings.Join(w, ",")
	}
	want := []string{"db", "api,worker", "web"}
	if len(got) != len(want) {
		t.Fatalf("waves = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("wave %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWavesSubsetIgnoresOutsideDeps(t *testing.T) {
	r := mustParse(t, graphRoot)
	// db not requested: web still deploys after api, api in wave 0.
	waves, err := r.Waves([]string{"web", "api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(waves) != 2 || waves[0][0] != "api" || waves[1][0] != "web" {
		t.Errorf("subset waves = %v", waves)
	}
}

func TestCycleRejected(t *testing.T) {
	yml := strings.Replace(graphRoot, "db:     { path: apps/db,     servers: [s1] }",
		"db:     { path: apps/db,     servers: [s1], depends_on: [web] }", 1)
	if _, err := ParseRoot([]byte(yml)); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got %v", err)
	}
}

func TestUnknownDependencyRejected(t *testing.T) {
	yml := strings.Replace(graphRoot, "depends_on: [db] }\n  web", "depends_on: [nope] }\n  web", 1)
	if _, err := ParseRoot([]byte(yml)); err == nil {
		t.Error("expected unknown depends_on error")
	}
}

func TestBlueGreenValidation(t *testing.T) {
	if _, err := ParseManifest([]byte("runtime: docker\nblue_green: { ports: [8001, 8002] }"), "web"); err != nil {
		t.Errorf("valid blue_green rejected: %v", err)
	}
	if _, err := ParseManifest([]byte("runtime: docker\nblue_green: { ports: [8001] }"), "web"); err == nil {
		t.Error("one port should be rejected")
	}
	if _, err := ParseManifest([]byte("runtime: script\nscripts: { start: x }\nblue_green: { ports: [1, 2] }"), "web"); err == nil {
		t.Error("blue_green on script runtime should be rejected")
	}
}

func TestServerOSDefaults(t *testing.T) {
	yml := strings.Replace(graphRoot, "s1: { host: h, user: u }", "s1: { host: h, user: u, os: windows }", 1)
	r := mustParse(t, yml)
	s := r.Servers["s1"]
	if s.Root != "C:/f1" || s.F1Bin() != "C:/f1/bin/f1.exe" {
		t.Errorf("windows defaults wrong: root=%q bin=%q", s.Root, s.F1Bin())
	}
}
