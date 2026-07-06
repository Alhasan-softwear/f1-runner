// Package provision installs runtime dependencies on servers: language
// runtimes (python, node, php), web servers (apache, nginx), databases
// (mariadb, postgres, redis), and docker itself. Idempotent: each package has
// a cheap presence check and is skipped when already installed.
//
// Linux only (apt, apk, dnf/yum). Runs as root, or via passwordless sudo.
package provision

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

// recipe describes one logical package across package managers.
type recipe struct {
	check string              // command that exits 0 when already installed
	pkgs  map[string][]string // manager -> distro packages
	pre   map[string][]string // manager -> commands to run before install (e.g. add repo)
	post  map[string][]string // manager -> commands after install (enable services…); best-effort
}

// service enable+start that works with or without systemd, quietly.
func svc(name string) string {
	return fmt.Sprintf("(command -v systemctl >/dev/null && systemctl enable --now %s || rc-service %s start || service %s start) >/dev/null 2>&1 || true", name, name, name)
}

var recipes = map[string]recipe{
	"python": {
		// --help passes even when ensurepip is missing, so actually create a
		// throwaway venv: that is what fails on a bare Ubuntu python3.
		check: `command -v python3 && python3 -c 'import venv,ensurepip' && d=$(mktemp -d) && python3 -m venv "$d/v" >/dev/null 2>&1; rc=$?; rm -rf "$d"; [ $rc -eq 0 ]`,
		pkgs: map[string][]string{
			"apt": {"python3", "python3-pip", "python3-venv", "python3-dev"},
			"apk": {"python3", "py3-pip"},
			"dnf": {"python3", "python3-pip"},
		},
	},
	"node": {
		check: "command -v node",
		pkgs: map[string][]string{
			"apt": {"nodejs", "npm"},
			"apk": {"nodejs", "npm"},
			"dnf": {"nodejs", "npm"},
		},
		// node@<major> on apt uses NodeSource for the exact major version.
	},
	"php": {
		check: "command -v php",
		pkgs: map[string][]string{
			"apt": {"php", "php-cli", "php-fpm", "php-mysql", "php-xml", "php-mbstring", "php-curl", "php-zip", "php-gd"},
			"apk": {"php83", "php83-fpm", "php83-pdo_mysql", "php83-mysqli", "php83-mbstring", "php83-curl", "php83-xml", "php83-gd", "php83-session", "php83-json"},
			"dnf": {"php", "php-cli", "php-fpm", "php-mysqlnd", "php-xml", "php-mbstring", "php-gd"},
		},
		post: map[string][]string{
			"apt": {svc("php-fpm") /* name varies by version; best-effort */},
			"apk": {svc("php-fpm83")},
			"dnf": {svc("php-fpm")},
		},
	},
	"apache": {
		check: "command -v apache2ctl || command -v httpd",
		pkgs: map[string][]string{
			"apt": {"apache2"},
			"apk": {"apache2", "apache2-proxy"},
			"dnf": {"httpd"},
		},
		post: map[string][]string{
			"apt": {svc("apache2")},
			"apk": {svc("apache2")},
			"dnf": {svc("httpd")},
		},
	},
	"nginx": {
		check: "command -v nginx",
		pkgs: map[string][]string{
			"apt": {"nginx"}, "apk": {"nginx"}, "dnf": {"nginx"},
		},
		post: map[string][]string{
			"apt": {svc("nginx")}, "apk": {svc("nginx")}, "dnf": {svc("nginx")},
		},
	},
	"mariadb": {
		check: "command -v mariadb || command -v mysql",
		pkgs: map[string][]string{
			"apt": {"mariadb-server", "mariadb-client"},
			"apk": {"mariadb", "mariadb-client"},
			"dnf": {"mariadb-server"},
		},
		post: map[string][]string{
			"apt": {svc("mariadb")},
			"apk": {"[ -d /var/lib/mysql/mysql ] || mariadb-install-db --user=mysql --datadir=/var/lib/mysql >/dev/null 2>&1 || true", svc("mariadb")},
			"dnf": {svc("mariadb")},
		},
	},
	"postgres": {
		check: "command -v psql",
		pkgs: map[string][]string{
			"apt": {"postgresql"},
			"apk": {"postgresql"},
			"dnf": {"postgresql-server"},
		},
		post: map[string][]string{
			"apt": {svc("postgresql")},
			"apk": {svc("postgresql")},
			"dnf": {"postgresql-setup --initdb >/dev/null 2>&1 || true", svc("postgresql")},
		},
	},
	"redis": {
		check: "command -v redis-server",
		pkgs: map[string][]string{
			"apt": {"redis-server"}, "apk": {"redis"}, "dnf": {"redis"},
		},
		post: map[string][]string{
			"apt": {svc("redis-server")}, "apk": {svc("redis")}, "dnf": {svc("redis")},
		},
	},
	"docker": {
		check: "command -v docker",
		pkgs: map[string][]string{
			"apt": {}, // installed via get.docker.com (includes compose plugin)
			"apk": {"docker", "docker-cli-compose"},
			"dnf": {},
		},
		pre: map[string][]string{
			"apt": {"curl -fsSL https://get.docker.com | sh"},
			"dnf": {"curl -fsSL https://get.docker.com | sh"},
		},
		post: map[string][]string{
			"apt": {svc("docker")}, "apk": {svc("docker")}, "dnf": {svc("docker")},
		},
	},
	"git":  {check: "command -v git", pkgs: map[string][]string{"apt": {"git"}, "apk": {"git"}, "dnf": {"git"}}},
	"curl": {check: "command -v curl", pkgs: map[string][]string{"apt": {"curl"}, "apk": {"curl"}, "dnf": {"curl"}}},
	"build": {
		check: "command -v gcc && command -v make",
		pkgs: map[string][]string{
			"apt": {"build-essential"}, "apk": {"alpine-sdk"}, "dnf": {"gcc", "gcc-c++", "make"},
		},
	},
}

// Known returns the logical package names f1 can provision.
func Known() []string {
	names := make([]string, 0, len(recipes))
	for n := range recipes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Normalize resolves aliases and validates a requested package spec
// ("name" or "name@version"). Returns name, version.
func Normalize(spec string) (string, string, error) {
	name, version, _ := strings.Cut(strings.TrimSpace(strings.ToLower(spec)), "@")
	switch name {
	case "mysql":
		name = "mariadb"
	case "nodejs":
		name = "node"
	case "postgresql":
		name = "postgres"
	case "python3":
		name = "python"
	}
	if _, ok := recipes[name]; !ok {
		return "", "", fmt.Errorf("unknown provision package %q (known: %s)", spec, strings.Join(Known(), ", "))
	}
	return name, version, nil
}

func detectManager() (string, error) {
	for _, m := range []struct{ name, bin string }{
		{"apt", "apt-get"}, {"apk", "apk"}, {"dnf", "dnf"}, {"dnf", "yum"},
	} {
		if _, err := exec.LookPath(m.bin); err == nil {
			return m.name, nil
		}
	}
	return "", fmt.Errorf("no supported package manager found (apt-get, apk, dnf, or yum)")
}

func installCmd(manager string, pkgs []string) string {
	list := strings.Join(pkgs, " ")
	switch manager {
	case "apt":
		return "DEBIAN_FRONTEND=noninteractive apt-get install -y -q " + list
	case "apk":
		return "apk add --no-cache " + list
	default:
		bin := "dnf"
		if _, err := exec.LookPath("dnf"); err != nil {
			bin = "yum"
		}
		return bin + " install -y -q " + list
	}
}

func updateCmd(manager string) string {
	switch manager {
	case "apt":
		return "apt-get update -q"
	case "apk":
		return "true" // --no-cache fetches fresh indexes
	default:
		return "true"
	}
}

// Ensure installs every requested package that isn't already present.
// Specs are logical names, optionally versioned ("node@22").
func Ensure(specs []string, out io.Writer) error {
	if len(specs) == 0 {
		return nil
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("provisioning is only supported on linux servers")
	}
	sudo := ""
	if os.Geteuid() != 0 {
		if exec.Command("sudo", "-n", "true").Run() != nil {
			return fmt.Errorf("provisioning needs root or passwordless sudo")
		}
		sudo = "sudo -n "
	}
	manager, err := detectManager()
	if err != nil {
		return err
	}

	run := func(cmd string) error {
		c := exec.Command("sh", "-c", sudo+"sh -c "+shQuote(cmd))
		c.Stdout = out
		c.Stderr = out
		return c.Run()
	}
	check := func(cmd string) bool {
		return exec.Command("sh", "-c", cmd+" >/dev/null 2>&1").Run() == nil
	}

	updated := false
	var failed []string
	for _, spec := range specs {
		name, version, err := Normalize(spec)
		if err != nil {
			return err
		}
		r := recipes[name]
		if check(r.check) {
			fmt.Fprintf(out, "%s: already installed\n", name)
			continue
		}
		fmt.Fprintf(out, "%s: installing (%s)…\n", name, manager)

		pre := r.pre[manager]
		pkgs := r.pkgs[manager]
		// node@<major>: pin via NodeSource on apt/dnf. Its nodejs package
		// bundles npm and CONFLICTS with the distro npm package, so install
		// nodejs alone here.
		if name == "node" && version != "" && manager != "apk" {
			pre = []string{fmt.Sprintf("curl -fsSL https://deb.nodesource.com/setup_%s.x | bash -", version)}
			if manager == "dnf" {
				pre = []string{fmt.Sprintf("curl -fsSL https://rpm.nodesource.com/setup_%s.x | bash -", version)}
			}
			pkgs = []string{"nodejs"}
		} else if version != "" {
			fmt.Fprintf(out, "%s: note — version pinning (@%s) is only honored for node on apt/dnf; installing the distro version\n", name, version)
		}

		if !updated && (len(pkgs) > 0 || len(pre) > 0) {
			run(updateCmd(manager)) // best effort
			updated = true
		}
		ok := true
		for _, p := range pre {
			if err := run(p); err != nil {
				fmt.Fprintf(out, "%s: pre-step failed: %v\n", name, err)
				ok = false
				break
			}
		}
		if ok && len(pkgs) > 0 {
			if err := run(installCmd(manager, pkgs)); err != nil {
				fmt.Fprintf(out, "%s: install failed: %v\n", name, err)
				ok = false
			}
		}
		if ok {
			for _, p := range r.post[manager] {
				run(p) // best effort by design
			}
		}
		if !ok || !check(r.check) {
			failed = append(failed, name)
			continue
		}
		fmt.Fprintf(out, "%s: ready\n", name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not provision: %s", strings.Join(failed, ", "))
	}
	return nil
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
