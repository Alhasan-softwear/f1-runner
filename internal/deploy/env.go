// f1 env — manage per-component env files on servers without ever putting
// secrets in the repo. Files live at <root>/env/<comp>.env and are loaded
// into every lifecycle command's environment at deploy time.
package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	gorun "runtime"
	"strings"

	"github.com/Alhasan-softwear/f1-runner/internal/config"
	"github.com/Alhasan-softwear/f1-runner/internal/release"
	"github.com/Alhasan-softwear/f1-runner/internal/sshx"
	"github.com/Alhasan-softwear/f1-runner/internal/ui"
)

// EnvSetLocal upserts KEY=VALUE pairs in the component's env file, preserving
// unrelated lines and comments.
func EnvSetLocal(root, comp string, pairs []string) error {
	kv := map[string]string{}
	var order []string
	for _, p := range pairs {
		key, val, ok := strings.Cut(p, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return fmt.Errorf("expected KEY=VALUE, got %q", p)
		}
		if _, seen := kv[key]; !seen {
			order = append(order, key)
		}
		kv[key] = val
	}
	return editEnvFile(root, comp, func(lines []string) []string {
		seen := map[string]bool{}
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			key, _, ok := strings.Cut(strings.TrimPrefix(trimmed, "export "), "=")
			key = strings.TrimSpace(key)
			if ok {
				if val, hit := kv[key]; hit {
					lines[i] = key + "=" + val
					seen[key] = true
				}
			}
		}
		for _, key := range order {
			if !seen[key] {
				lines = append(lines, key+"="+kv[key])
			}
		}
		return lines
	})
}

// EnvUnsetLocal removes keys from the component's env file.
func EnvUnsetLocal(root, comp string, keys []string) error {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[strings.TrimSpace(k)] = true
	}
	return editEnvFile(root, comp, func(lines []string) []string {
		kept := lines[:0]
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			key, _, ok := strings.Cut(strings.TrimPrefix(trimmed, "export "), "=")
			if ok && drop[strings.TrimSpace(key)] && !strings.HasPrefix(trimmed, "#") {
				continue
			}
			kept = append(kept, line)
		}
		return kept
	})
}

// EnvShowLocal prints the component's env file.
func EnvShowLocal(root, comp string) error {
	path := release.Layout{Root: root}.EnvFile(comp)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		ui.Printf("(empty — no %s yet)", path)
		return nil
	}
	if err != nil {
		return err
	}
	os.Stdout.Write(raw)
	return nil
}

func editEnvFile(root, comp string, edit func([]string) []string) error {
	path := release.Layout{Root: root}.EnvFile(comp)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var lines []string
	if raw, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	lines = edit(lines)
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	mode := os.FileMode(0o600) // secrets: owner-only where the OS supports it
	if gorun.GOOS == "windows" {
		mode = 0o644
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Env fans an env operation out to the servers hosting the component.
func Env(cfg *config.Root, op, comp, serverFilter string, args []string) error {
	if err := sshx.CheckBinaries(); err != nil {
		return err
	}
	servers, err := serversFor(cfg, comp, serverFilter)
	if err != nil {
		return err
	}
	var failed []string
	for _, sv := range servers {
		target := sshx.Target{Name: sv, Server: cfg.Servers[sv]}
		out := ui.NewPrefix(sv)
		words := []string{target.Server.F1Bin(), "env", op, comp, "--local", "--root", target.Server.Root}
		words = append(words, args...)
		if err := target.Run(target.RemoteCmd(words), out, false); err != nil {
			out.Failf("env %s failed: %v", op, err)
			failed = append(failed, sv)
		} else if op != "show" {
			out.Okf("env %s done (takes effect on the next deploy)", op)
		}
		out.Flush()
	}
	if len(failed) > 0 {
		return fmt.Errorf("env %s failed on: %s", op, strings.Join(failed, ", "))
	}
	return nil
}
