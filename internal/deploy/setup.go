// f1 server setup — prepare a server: directory layout, the f1 binary
// itself, a git deploy key, and any packages listed under `provision`.
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
	Binary  string   // explicit local path to an uploadable f1 binary
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
	goos, goarch, err := detectPlatform(target, s)
	if err != nil {
		return err
	}
	out.Notef("server platform: %s/%s", goos, goarch)

	binary, err := findUploadBinary(opts.Binary, goos, goarch)
	if err != nil {
		return err
	}
	out.Notef("using local binary %s", binary)

	out.Step("creating %s layout", s.Root)
	var mkdir string
	if s.IsWindows() {
		mkdir = fmt.Sprintf("powershell -NoProfile -Command \"New-Item -ItemType Directory -Force -Path '%s/bin','%s/apps','%s/env' | Out-Null\"", s.Root, s.Root, s.Root)
	} else {
		mkdir = fmt.Sprintf("mkdir -p %s/bin %s/apps %s/env", sshx.Quote(s.Root), sshx.Quote(s.Root), sshx.Quote(s.Root))
	}
	if err := target.Run(mkdir, out, false); err != nil {
		return fmt.Errorf("cannot create %s as %s — on linux run once:  sudo mkdir -p %s && sudo chown %s %s",
			s.Root, s.User, s.Root, s.User, s.Root)
	}

	out.Step("installing f1 binary")
	remoteBin := s.F1Bin()
	tmp := remoteBin + ".new"
	if err := target.Upload(binary, tmp, out); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	var install string
	if s.IsWindows() {
		install = fmt.Sprintf("powershell -NoProfile -Command \"Move-Item -Force '%s' '%s'\"", tmp, remoteBin)
	} else {
		install = fmt.Sprintf("chmod +x %s && mv %s %s", sshx.Quote(tmp), sshx.Quote(tmp), sshx.Quote(remoteBin))
	}
	if err := target.Run(install, out, false); err != nil {
		return err
	}
	if v, err := target.Output(target.RemoteCmd([]string{remoteBin, "version"}), out); err == nil {
		out.Okf("f1 %s installed", strings.TrimSpace(v))
	} else {
		return fmt.Errorf("installed binary does not run — wrong platform? (server is %s/%s)", goos, goarch)
	}

	// A deploy key only matters when the repo is remote (git@… or ssh://…).
	if strings.Contains(cfg.Repo, "@") || strings.Contains(cfg.Repo, "://") {
		out.Step("deploy key")
		keyPath := s.Root + "/deploy_key"
		var check string
		if s.IsWindows() {
			check = fmt.Sprintf("powershell -NoProfile -Command \"if (-not (Test-Path '%s')) { ssh-keygen -t ed25519 -N '\\\"\\\"' -q -f '%s' -C f1@%s }\"", keyPath, keyPath, name)
		} else {
			check = fmt.Sprintf("test -f %s || ssh-keygen -t ed25519 -N '' -q -f %s -C f1@%s", sshx.Quote(keyPath), sshx.Quote(keyPath), name)
		}
		if err := target.Run(check, out, false); err != nil {
			return fmt.Errorf("could not create deploy key: %w", err)
		}
		catCmd := "cat " + sshx.Quote(keyPath+".pub")
		if s.IsWindows() {
			catCmd = fmt.Sprintf("powershell -NoProfile -Command \"Get-Content '%s.pub'\"", keyPath)
		}
		pub, err := target.Output(catCmd, out)
		if err != nil {
			return err
		}
		out.Okf("add this read-only deploy key to your repo host for %s:", cfg.Repo)
		ui.Printf("\n    %s", strings.TrimSpace(pub))
		ui.Printf("")
	} else {
		out.Notef("repo %q looks server-local — skipping deploy key", cfg.Repo)
	}

	if len(s.Provision) > 0 || len(provisionFromManifests(cfg, name)) > 0 {
		pkgs := dedupe(append(append([]string{}, s.Provision...), provisionFromManifests(cfg, name)...))
		if s.IsWindows() {
			out.Notef("provisioning is linux-only — skipping %s on this windows server", strings.Join(pkgs, ", "))
		} else {
			out.Step("provisioning: %s", strings.Join(pkgs, ", "))
			words := []string{remoteBin, "provision", "--local", strings.Join(pkgs, ",")}
			if err := target.Run(target.RemoteCmd(words), out, false); err != nil {
				return fmt.Errorf("provisioning failed: %w", err)
			}
		}
	}

	out.Step("checking server tools")
	tools, _ := target.Output("git --version 2>/dev/null; docker --version 2>/dev/null; docker compose version 2>/dev/null || docker-compose --version 2>/dev/null", out)
	for _, line := range strings.Split(strings.TrimSpace(tools), "\n") {
		if line != "" {
			out.Notef("%s", line)
		}
	}
	if !strings.Contains(tools, "git version") {
		return fmt.Errorf("git is not installed on the server — add 'git' to the server's provision list or install it manually")
	}
	if !strings.Contains(tools, "Docker version") {
		out.Notef("docker not found — fine for script components; add 'docker' to provision for docker ones")
	}
	out.Okf("server ready — deploy with: f1 deploy --all")
	return nil
}

// provisionFromManifests unions the provision lists of the components
// assigned to this server, read from the local monorepo working tree (dev
// side has it; the deploy itself re-checks per manifest at apply time).
func provisionFromManifests(cfg *config.Root, server string) []string {
	var pkgs []string
	for name, comp := range cfg.Components {
		hosted := false
		for _, sv := range comp.Servers {
			if sv == server {
				hosted = true
				break
			}
		}
		if !hosted {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(filepath.FromSlash(comp.Path), "f1.yml"))
		if err != nil {
			continue // not fatal: manifest read happens authoritatively at apply time
		}
		if m, err := config.ParseManifest(raw, name); err == nil {
			pkgs = append(pkgs, m.Provision...)
		}
	}
	return pkgs
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// detectPlatform finds the server's OS/arch. Linux via uname; Windows via
// cmd's PROCESSOR_ARCHITECTURE (works whether the ssh shell is cmd or
// powershell).
func detectPlatform(target sshx.Target, s config.Server) (string, string, error) {
	if s.IsWindows() {
		arch, err := target.Output("cmd /c echo %PROCESSOR_ARCHITECTURE%", nil)
		if err != nil {
			return "", "", fmt.Errorf("cannot ssh to %s@%s:%d — check host, user, and keys", s.User, s.Host, s.Port)
		}
		switch strings.TrimSpace(strings.ToUpper(arch)) {
		case "AMD64":
			return "windows", "amd64", nil
		case "ARM64":
			return "windows", "arm64", nil
		}
		return "", "", fmt.Errorf("unsupported windows architecture %q", strings.TrimSpace(arch))
	}
	arch, err := target.Output("uname -m", nil)
	if err != nil {
		return "", "", fmt.Errorf("cannot ssh to %s@%s:%d — check host, user, and keys (for windows servers set os: windows)", s.User, s.Host, s.Port)
	}
	goarch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[strings.TrimSpace(arch)]
	if goarch == "" {
		return "", "", fmt.Errorf("unsupported server architecture %q", strings.TrimSpace(arch))
	}
	return "linux", goarch, nil
}

// findUploadBinary locates a cross-compiled binary to upload: the --binary
// flag, or dist/f1-<os>-<arch>[.exe] next to the executable or under the
// current directory.
func findUploadBinary(explicit, goos, goarch string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--binary %s: %w", explicit, err)
		}
		return explicit, nil
	}
	want := "f1-" + goos + "-" + goarch
	if goos == "windows" {
		want += ".exe"
	}
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
	return "", fmt.Errorf("no %s found (looked next to f1 and in ./dist) — grab it from the repo's dist/ folder, build with `make dist`, or pass --binary", want)
}
