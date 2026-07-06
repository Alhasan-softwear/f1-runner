// f1 server setup — prepare a server: directory layout, the f1 binary
// itself, and a git deploy key.
package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Alhasan-softwear/f1-runner/internal/config"
	"github.com/Alhasan-softwear/f1-runner/internal/sshx"
	"github.com/Alhasan-softwear/f1-runner/internal/ui"
)

type SetupOptions struct {
	Servers []string // empty = all
	Binary  string   // explicit local path to a linux f1 binary
}

func ServerSetup(cfg *config.Root, opts SetupOptions) error {
	if err := sshx.CheckBinaries(); err != nil {
		return err
	}
	names := opts.Servers
	if len(names) == 0 {
		names = cfg.ServerNames()
	}
	var failed []string
	for _, sv := range names {
		s, ok := cfg.Servers[sv]
		if !ok {
			return fmt.Errorf("unknown server %q (defined: %s)", sv, strings.Join(cfg.ServerNames(), ", "))
		}
		if err := setupOne(cfg, sv, s, opts); err != nil {
			ui.Errorf("%s: %v", sv, err)
			failed = append(failed, sv)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("setup failed on: %s", strings.Join(failed, ", "))
	}
	return nil
}

func setupOne(cfg *config.Root, name string, s config.Server, opts SetupOptions) error {
	target := sshx.Target{Name: name, Server: s}
	out := ui.NewPrefix(name)
	defer out.Flush()

	out.Step("checking connection and architecture")
	arch, err := target.Output("uname -m", out)
	if err != nil {
		return fmt.Errorf("cannot ssh to %s@%s:%d — check host, user, and keys", s.User, s.Host, s.Port)
	}
	goarch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[strings.TrimSpace(arch)]
	if goarch == "" {
		return fmt.Errorf("unsupported server architecture %q", strings.TrimSpace(arch))
	}

	binary, err := findLinuxBinary(opts.Binary, goarch)
	if err != nil {
		return err
	}
	out.Notef("using local binary %s", binary)

	out.Step("creating %s layout", s.Root)
	mkdir := fmt.Sprintf("mkdir -p %s/bin %s/apps %s/env", sshx.Quote(s.Root), sshx.Quote(s.Root), sshx.Quote(s.Root))
	if err := target.Run(mkdir, out, false); err != nil {
		return fmt.Errorf("cannot create %s as %s — run once on the server:  sudo mkdir -p %s && sudo chown %s %s",
			s.Root, s.User, s.Root, s.User, s.Root)
	}

	out.Step("installing f1 binary")
	tmp := s.Root + "/bin/f1.new"
	if err := target.Upload(binary, tmp, out); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	install := fmt.Sprintf("chmod +x %s && mv %s %s", sshx.Quote(tmp), sshx.Quote(tmp), sshx.Quote(s.Root+"/bin/f1"))
	if err := target.Run(install, out, false); err != nil {
		return err
	}
	if v, err := target.Output(sshx.Quote(s.Root+"/bin/f1")+" version", out); err == nil {
		out.Okf("f1 %s installed", strings.TrimSpace(v))
	} else {
		return fmt.Errorf("installed binary does not run — wrong architecture? (server is %s)", strings.TrimSpace(arch))
	}

	// A deploy key only matters when the repo is remote (git@… or ssh://…).
	if strings.Contains(cfg.Repo, "@") || strings.Contains(cfg.Repo, "://") {
		out.Step("deploy key")
		keyPath := s.Root + "/deploy_key"
		check := fmt.Sprintf("test -f %s || ssh-keygen -t ed25519 -N '' -q -f %s -C f1@%s", sshx.Quote(keyPath), sshx.Quote(keyPath), name)
		if err := target.Run(check, out, false); err != nil {
			return fmt.Errorf("could not create deploy key: %w", err)
		}
		pub, err := target.Output("cat "+sshx.Quote(keyPath+".pub"), out)
		if err != nil {
			return err
		}
		out.Okf("add this read-only deploy key to your repo host for %s:", cfg.Repo)
		ui.Printf("\n    %s", strings.TrimSpace(pub))
		ui.Printf("")
	} else {
		out.Notef("repo %q looks server-local — skipping deploy key", cfg.Repo)
	}

	out.Step("checking server tools")
	tools, _ := target.Output("git --version 2>/dev/null; docker --version 2>/dev/null; docker compose version 2>/dev/null || docker-compose --version 2>/dev/null", out)
	for _, line := range strings.Split(strings.TrimSpace(tools), "\n") {
		if line != "" {
			out.Notef("%s", line)
		}
	}
	if !strings.Contains(tools, "git version") {
		return fmt.Errorf("git is not installed on the server — install it (apt install git / apk add git)")
	}
	if !strings.Contains(tools, "Docker version") {
		out.Notef("docker not found — fine for script components, required for docker ones")
	}
	out.Okf("server ready — deploy with: f1 deploy --all")
	return nil
}

// findLinuxBinary locates a cross-compiled linux binary to upload: the
// --binary flag, or dist/f1-linux-<arch> next to the executable or under the
// current directory.
func findLinuxBinary(explicit, goarch string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--binary %s: %w", explicit, err)
		}
		return explicit, nil
	}
	want := "f1-linux-" + goarch
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, want), filepath.Join(dir, "dist", want))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "dist", want), filepath.Join(wd, want))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no linux binary found (looked for %s next to f1 and in ./dist) — build one with `make dist` or pass --binary", want)
}
