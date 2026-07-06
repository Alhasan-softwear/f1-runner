// Package config loads and validates the root f1.yml (repo-level) and
// per-component f1.yml manifests.
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Root is the repo-level f1.yml: which servers exist and which component
// lives where.
type Root struct {
	Project    string               `yaml:"project"`
	Repo       string               `yaml:"repo"`
	Branch     string               `yaml:"branch"` // default deploy ref, defaults to "main"
	Servers    map[string]Server    `yaml:"servers"`
	Components map[string]Component `yaml:"components"`
}

// Server is one deploy target reachable over SSH.
type Server struct {
	Host    string   `yaml:"host"`
	User    string   `yaml:"user"`
	Port    int      `yaml:"port"`     // default 22
	Key     string   `yaml:"key"`      // optional identity file (-i)
	Root    string   `yaml:"root"`     // f1 root on the server, default /opt/f1
	SSHOpts []string `yaml:"ssh_opts"` // extra raw ssh options, e.g. ["-o","StrictHostKeyChecking=accept-new"]
}

// Component maps a monorepo subdirectory onto one or more servers.
type Component struct {
	Path    string   `yaml:"path"`
	Servers []string `yaml:"servers"`
}

// Manifest is a component's own f1.yml, read from the repo at the deployed
// ref (never from the working tree), so a deploy always uses the manifest as
// of the commit it ships.
type Manifest struct {
	Name    string  `yaml:"name"` // optional; must match the root config key if set
	Runtime string  `yaml:"runtime"` // "docker" or "script"
	Docker  Docker  `yaml:"docker"`
	EnvFile string  `yaml:"env_file"` // server-side path, e.g. /opt/f1/env/web.env
	Health  Health  `yaml:"health"`
	Scripts Scripts `yaml:"scripts"`
	Keep    int     `yaml:"keep"` // releases to keep, default 5
}

type Docker struct {
	Compose string `yaml:"compose"` // compose file relative to the component dir, default docker-compose.yml
}

type Scripts struct {
	Setup string `yaml:"setup"`
	Build string `yaml:"build"`
	Start string `yaml:"start"`
	Stop  string `yaml:"stop"`
	Logs  string `yaml:"logs"`
}

type Health struct {
	Cmd      string `yaml:"cmd"`      // shell command; exit 0 = healthy
	URL      string `yaml:"url"`      // or an HTTP GET that must return 2xx/3xx
	Retries  int    `yaml:"retries"`  // default 5
	Interval string `yaml:"interval"` // default 3s
}

func (h Health) Defined() bool { return h.Cmd != "" || h.URL != "" }

func (h Health) RetriesOrDefault() int {
	if h.Retries <= 0 {
		return 5
	}
	return h.Retries
}

func (h Health) IntervalOrDefault() time.Duration {
	d, err := time.ParseDuration(h.Interval)
	if err != nil || d <= 0 {
		return 3 * time.Second
	}
	return d
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// LoadRoot reads and validates a root f1.yml from disk.
func LoadRoot(path string) (*Root, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseRoot(raw)
}

// ParseRoot parses and validates root f1.yml content.
func ParseRoot(raw []byte) (*Root, error) {
	var r Root
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("f1.yml: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *Root) Validate() error {
	if r.Project == "" {
		return fmt.Errorf("f1.yml: 'project' is required")
	}
	if r.Repo == "" {
		return fmt.Errorf("f1.yml: 'repo' is required (git URL or server-local path)")
	}
	if r.Branch == "" {
		r.Branch = "main"
	}
	if len(r.Components) == 0 {
		return fmt.Errorf("f1.yml: at least one component is required")
	}
	for name, s := range r.Servers {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("f1.yml: server name %q is invalid (use lowercase letters, digits, - or _)", name)
		}
		if s.Host == "" {
			return fmt.Errorf("f1.yml: server %q needs a 'host'", name)
		}
		if s.User == "" {
			return fmt.Errorf("f1.yml: server %q needs a 'user'", name)
		}
		if s.Port == 0 {
			s.Port = 22
		}
		if s.Root == "" {
			s.Root = "/opt/f1"
		}
		r.Servers[name] = s
	}
	for name, c := range r.Components {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("f1.yml: component name %q is invalid (use lowercase letters, digits, - or _)", name)
		}
		p := strings.TrimSuffix(strings.ReplaceAll(c.Path, "\\", "/"), "/")
		if p == "" || p == "." {
			return fmt.Errorf("f1.yml: component %q needs a 'path' that is a subdirectory of the repo (not the repo root)", name)
		}
		if strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			return fmt.Errorf("f1.yml: component %q path %q must be relative and inside the repo", name, c.Path)
		}
		if len(c.Servers) == 0 {
			return fmt.Errorf("f1.yml: component %q needs at least one entry in 'servers'", name)
		}
		for _, sv := range c.Servers {
			if _, ok := r.Servers[sv]; !ok {
				return fmt.Errorf("f1.yml: component %q references unknown server %q (known: %s)", name, sv, strings.Join(r.ServerNames(), ", "))
			}
		}
		c.Path = p
		r.Components[name] = c
	}
	return nil
}

// ComponentNames returns component names sorted for stable output.
func (r *Root) ComponentNames() []string {
	names := make([]string, 0, len(r.Components))
	for n := range r.Components {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (r *Root) ServerNames() []string {
	names := make([]string, 0, len(r.Servers))
	for n := range r.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ParseManifest parses and validates a component f1.yml.
func ParseManifest(raw []byte, component string) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s/f1.yml: %w", component, err)
	}
	if m.Name != "" && m.Name != component {
		return nil, fmt.Errorf("%s/f1.yml: 'name: %s' does not match the component key %q in the root f1.yml", component, m.Name, component)
	}
	m.Name = component
	switch m.Runtime {
	case "docker":
		if m.Docker.Compose == "" {
			m.Docker.Compose = "docker-compose.yml"
		}
	case "script":
		if m.Scripts.Start == "" {
			return nil, fmt.Errorf("%s/f1.yml: runtime 'script' requires scripts.start", component)
		}
	case "":
		return nil, fmt.Errorf("%s/f1.yml: 'runtime' is required (docker or script)", component)
	default:
		return nil, fmt.Errorf("%s/f1.yml: unknown runtime %q (use docker or script)", component, m.Runtime)
	}
	if m.Keep <= 0 {
		m.Keep = 5
	}
	return &m, nil
}
