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
	Host      string   `yaml:"host"`
	User      string   `yaml:"user"`
	Port      int      `yaml:"port"`      // default 22
	Key       string   `yaml:"key"`       // optional identity file (-i)
	Root      string   `yaml:"root"`      // f1 root on the server, default /opt/f1 (C:/f1 on windows)
	OS        string   `yaml:"os"`        // "linux" (default) or "windows" (experimental)
	Provision []string `yaml:"provision"` // packages to install during `f1 server setup`
	SSHOpts   []string `yaml:"ssh_opts"`  // extra raw ssh options
}

func (s Server) IsWindows() bool { return s.OS == "windows" }

// F1Bin is the path of the f1 binary on this server.
func (s Server) F1Bin() string {
	if s.IsWindows() {
		return s.Root + "/bin/f1.exe"
	}
	return s.Root + "/bin/f1"
}

// Component maps a monorepo subdirectory onto one or more servers.
// Path may be "." to deploy the whole repo as one component — in that case
// Manifest must name a file other than f1.yml (which is the root config).
type Component struct {
	Path      string   `yaml:"path"`
	Manifest  string   `yaml:"manifest"` // manifest filename inside Path, default f1.yml
	Servers   []string `yaml:"servers"`
	DependsOn []string `yaml:"depends_on"` // deployed (and health-checked) before this one
}

// ManifestPath is the repo-relative path of this component's manifest.
func (c Component) ManifestPath() string {
	name := c.Manifest
	if name == "" {
		name = "f1.yml"
	}
	if c.Path == "." {
		return name
	}
	return c.Path + "/" + name
}

// Manifest is a component's own f1.yml, read from the repo at the deployed
// ref (never from the working tree), so a deploy always uses the manifest as
// of the commit it ships.
type Manifest struct {
	Name      string     `yaml:"name"`    // optional; must match the root config key if set
	Runtime   string     `yaml:"runtime"` // "docker" or "script"
	Docker    Docker     `yaml:"docker"`
	EnvFile   string     `yaml:"env_file"` // explicit server-side env file (required to exist if set)
	Health    Health     `yaml:"health"`
	Scripts   Scripts    `yaml:"scripts"`
	Shell     string     `yaml:"shell"`      // sh (default) | bash | cmd | powershell
	Provision []string   `yaml:"provision"`  // packages ensured on the server before deploying
	BlueGreen *BlueGreen `yaml:"blue_green"` // optional zero-downtime slot deploys (docker runtime)
	Keep      int        `yaml:"keep"`       // releases to keep, default 5
}

type Docker struct {
	Compose string `yaml:"compose"` // compose file relative to the component dir, default docker-compose.yml
}

// BlueGreen alternates deploys between two fixed ports ("blue" and "green"
// slots). The new slot must pass health before the switch hook runs and the
// old slot is stopped. Your reverse proxy (or the switch script) moves
// traffic between the two ports.
type BlueGreen struct {
	Ports  []int  `yaml:"ports"`  // exactly two, e.g. [8001, 8002]
	Env    string `yaml:"env"`    // env var carrying the slot port, default F1_PORT
	Switch string `yaml:"switch"` // optional hook after health, e.g. rewrite nginx upstream + reload
}

func (b *BlueGreen) EnvVar() string {
	if b.Env == "" {
		return "F1_PORT"
	}
	return b.Env
}

// SlotNames are fixed: slot 0 = blue (Ports[0]), slot 1 = green (Ports[1]).
var SlotNames = [2]string{"blue", "green"}

func (b *BlueGreen) SlotIndex(name string) int {
	if name == SlotNames[1] {
		return 1
	}
	return 0
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
	URL      string `yaml:"url"`      // or an HTTP GET that must return 2xx/3xx ($F1_PORT is substituted)
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
		switch s.OS {
		case "", "linux":
			s.OS = "linux"
		case "windows":
		default:
			return fmt.Errorf("f1.yml: server %q has unknown os %q (linux or windows)", name, s.OS)
		}
		if s.Port == 0 {
			s.Port = 22
		}
		if s.Root == "" {
			if s.IsWindows() {
				s.Root = "C:/f1"
			} else {
				s.Root = "/opt/f1"
			}
		}
		r.Servers[name] = s
	}
	for name, c := range r.Components {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("f1.yml: component name %q is invalid (use lowercase letters, digits, - or _)", name)
		}
		p := strings.TrimSuffix(strings.ReplaceAll(c.Path, "\\", "/"), "/")
		if p == "" {
			return fmt.Errorf("f1.yml: component %q needs a 'path' (a subdirectory, or \".\" for the repo root)", name)
		}
		if p == "." && (c.Manifest == "" || c.Manifest == "f1.yml") {
			return fmt.Errorf("f1.yml: root component %q must set 'manifest:' to a filename other than f1.yml (e.g. manifest: f1.%s.yml)", name, name)
		}
		if p != "." && (strings.HasPrefix(p, "/") || strings.Contains(p, "..")) {
			return fmt.Errorf("f1.yml: component %q path %q must be relative and inside the repo", name, c.Path)
		}
		if m := c.Manifest; m != "" && (strings.ContainsAny(m, "/\\") || strings.Contains(m, "..")) {
			return fmt.Errorf("f1.yml: component %q manifest %q must be a plain filename inside its path", name, m)
		}
		if len(c.Servers) == 0 {
			return fmt.Errorf("f1.yml: component %q needs at least one entry in 'servers'", name)
		}
		for _, sv := range c.Servers {
			if _, ok := r.Servers[sv]; !ok {
				return fmt.Errorf("f1.yml: component %q references unknown server %q (known: %s)", name, sv, strings.Join(r.ServerNames(), ", "))
			}
		}
		for _, dep := range c.DependsOn {
			if _, ok := r.Components[dep]; !ok {
				return fmt.Errorf("f1.yml: component %q depends_on unknown component %q", name, dep)
			}
			if dep == name {
				return fmt.Errorf("f1.yml: component %q cannot depend on itself", name)
			}
		}
		c.Path = p
		r.Components[name] = c
	}
	// Surface dependency cycles at load time, not mid-deploy.
	if _, err := r.Waves(r.ComponentNames()); err != nil {
		return err
	}
	return nil
}

// Waves topologically sorts the requested components into deploy waves:
// wave N+1 components depend on something in waves 0..N. Dependencies outside
// the requested set are assumed already deployed and don't pull themselves in.
func (r *Root) Waves(requested []string) ([][]string, error) {
	inSet := map[string]bool{}
	for _, n := range requested {
		inSet[n] = true
	}
	depth := map[string]int{}
	var visit func(name string, chain []string) (int, error)
	visit = func(name string, chain []string) (int, error) {
		if d, ok := depth[name]; ok {
			if d == -1 {
				return 0, fmt.Errorf("f1.yml: dependency cycle: %s", strings.Join(append(chain, name), " -> "))
			}
			return d, nil
		}
		depth[name] = -1 // visiting
		max := 0
		for _, dep := range r.Components[name].DependsOn {
			if !inSet[dep] {
				continue
			}
			d, err := visit(dep, append(chain, name))
			if err != nil {
				return 0, err
			}
			if d+1 > max {
				max = d + 1
			}
		}
		depth[name] = max
		return max, nil
	}
	maxDepth := 0
	for _, n := range requested {
		d, err := visit(n, nil)
		if err != nil {
			return nil, err
		}
		if d > maxDepth {
			maxDepth = d
		}
	}
	waves := make([][]string, maxDepth+1)
	for _, n := range requested {
		waves[depth[n]] = append(waves[depth[n]], n)
	}
	for _, w := range waves {
		sort.Strings(w)
	}
	return waves, nil
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
	switch m.Shell {
	case "", "sh", "bash", "cmd", "powershell":
	default:
		return nil, fmt.Errorf("%s/f1.yml: unknown shell %q (sh, bash, cmd, or powershell)", component, m.Shell)
	}
	if m.BlueGreen != nil {
		if m.Runtime != "docker" {
			return nil, fmt.Errorf("%s/f1.yml: blue_green requires the docker runtime", component)
		}
		if len(m.BlueGreen.Ports) != 2 || m.BlueGreen.Ports[0] == m.BlueGreen.Ports[1] {
			return nil, fmt.Errorf("%s/f1.yml: blue_green.ports must be exactly two distinct ports, e.g. [8001, 8002]", component)
		}
	}
	if m.Keep <= 0 {
		m.Keep = 5
	}
	return &m, nil
}
