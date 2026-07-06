package config

import (
	"strings"
	"testing"
)

const goodRoot = `
project: demo
repo: git@github.com:me/demo.git
servers:
  web1: { host: 1.2.3.4, user: deploy }
components:
  web: { path: apps/web, servers: [web1] }
`

func TestParseRootDefaults(t *testing.T) {
	r, err := ParseRoot([]byte(goodRoot))
	if err != nil {
		t.Fatal(err)
	}
	if r.Branch != "main" {
		t.Errorf("branch default = %q, want main", r.Branch)
	}
	s := r.Servers["web1"]
	if s.Port != 22 || s.Root != "/opt/f1" {
		t.Errorf("server defaults not applied: %+v", s)
	}
}

func TestParseRootRejects(t *testing.T) {
	cases := map[string]string{
		"no project":     strings.Replace(goodRoot, "project: demo", "", 1),
		"no repo":        strings.Replace(goodRoot, "repo: git@github.com:me/demo.git", "", 1),
		"unknown server": strings.Replace(goodRoot, "servers: [web1]", "servers: [nope]", 1),
		"root path":      strings.Replace(goodRoot, "path: apps/web", "path: .", 1),
		"absolute path":  strings.Replace(goodRoot, "path: apps/web", "path: /etc", 1),
		"escaping path":  strings.Replace(goodRoot, "path: apps/web", "path: ../up", 1),
		"bad comp name":  strings.Replace(goodRoot, "  web: {", "  WEB!: {", 1),
	}
	for name, yml := range cases {
		if _, err := ParseRoot([]byte(yml)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseManifest(t *testing.T) {
	m, err := ParseManifest([]byte("runtime: docker"), "web")
	if err != nil {
		t.Fatal(err)
	}
	if m.Docker.Compose != "docker-compose.yml" || m.Keep != 5 || m.Name != "web" {
		t.Errorf("defaults not applied: %+v", m)
	}
	if _, err := ParseManifest([]byte("runtime: script"), "web"); err == nil {
		t.Error("script without start should fail")
	}
	if _, err := ParseManifest([]byte("runtime: nope"), "web"); err == nil {
		t.Error("unknown runtime should fail")
	}
	if _, err := ParseManifest([]byte("name: other\nruntime: docker"), "web"); err == nil {
		t.Error("name mismatch should fail")
	}
}

func TestHealthDefaults(t *testing.T) {
	h := Health{Cmd: "true", Interval: "250ms", Retries: 2}
	if h.IntervalOrDefault().Milliseconds() != 250 || h.RetriesOrDefault() != 2 {
		t.Error("explicit health values not honored")
	}
	h = Health{}
	if h.Defined() {
		t.Error("empty health should not be defined")
	}
	if h.RetriesOrDefault() != 5 || h.IntervalOrDefault().Seconds() != 3 {
		t.Error("health defaults wrong")
	}
}
