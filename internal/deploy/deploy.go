// Dev-side orchestration: fan deploys and queries out to servers over SSH,
// where the remote f1 binary does the actual work.
package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Alhasan-softwear/f1-runner/internal/config"
	"github.com/Alhasan-softwear/f1-runner/internal/release"
	"github.com/Alhasan-softwear/f1-runner/internal/sshx"
	"github.com/Alhasan-softwear/f1-runner/internal/ui"
)

type DeployOptions struct {
	Components []string // empty + All=false is an error at the CLI layer
	All        bool
	Ref        string // default cfg.Branch
	Force      bool
	DryRun     bool
}

// Deploy resolves dependency waves, then runs `f1 apply` on every target
// server in parallel within each wave, with a barrier between waves so
// dependencies are live (and health-checked) before their dependents start.
func Deploy(cfg *config.Root, opts DeployOptions) error {
	if err := sshx.CheckBinaries(); err != nil {
		return err
	}
	targets := opts.Components
	if opts.All {
		targets = cfg.ComponentNames()
	}
	for _, name := range targets {
		if _, ok := cfg.Components[name]; !ok {
			return fmt.Errorf("unknown component %q (defined: %s)", name, strings.Join(cfg.ComponentNames(), ", "))
		}
	}
	waves, err := cfg.Waves(targets)
	if err != nil {
		return err
	}

	ref := opts.Ref
	if ref == "" {
		ref = cfg.Branch
	}
	// Pin the ref to a sha once, so every server (and every wave) deploys the
	// same commit even if someone pushes mid-deploy. Falls back to the ref
	// name when the repo isn't reachable from this machine.
	pinned := pinRef(cfg.Repo, ref)
	if pinned != ref {
		ui.Printf("%s %s -> %s", ui.Dim("ref"), ref, release.ShortSha(pinned))
	}

	for wi, wave := range waves {
		if len(waves) > 1 {
			ui.Printf("%s", ui.Bold(fmt.Sprintf("wave %d/%d: %s", wi+1, len(waves), strings.Join(wave, ", "))))
		}
		byServer := map[string][]string{}
		for _, name := range wave {
			for _, sv := range cfg.Components[name].Servers {
				byServer[sv] = append(byServer[sv], name)
			}
		}
		if err := deployWave(cfg, byServer, pinned, opts); err != nil {
			if wi < len(waves)-1 {
				return fmt.Errorf("%w — later waves were not deployed", err)
			}
			return err
		}
	}
	return nil
}

func deployWave(cfg *config.Root, byServer map[string][]string, ref string, opts DeployOptions) error {
	var wg sync.WaitGroup
	errs := make(map[string]error)
	var mu sync.Mutex
	for _, sv := range sortedKeys(byServer) {
		comps := byServer[sv]
		target := sshx.Target{Name: sv, Server: cfg.Servers[sv]}
		out := ui.NewPrefix(sv)
		words := []string{target.Server.F1Bin(), "apply",
			"--root", target.Server.Root,
			"--repo", cfg.Repo,
			"--ref", ref,
			"--components", strings.Join(comps, ","),
		}
		if opts.Force {
			words = append(words, "--force")
		}
		if opts.DryRun {
			words = append(words, "--dry-run")
		}
		wg.Add(1)
		go func(sv string) {
			defer wg.Done()
			defer out.Flush()
			out.Step("deploying %s", strings.Join(comps, ", "))
			err := target.Run(target.RemoteCmd(words), out, false)
			if err != nil {
				out.Failf("deploy failed: %v", err)
			} else {
				out.Okf("done")
			}
			mu.Lock()
			errs[sv] = err
			mu.Unlock()
		}(sv)
	}
	wg.Wait()

	var failed []string
	for sv, err := range errs {
		if err != nil {
			failed = append(failed, sv)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("deploy failed on: %s", strings.Join(failed, ", "))
	}
	return nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func pinRef(repoURL, ref string) string {
	if shaRe.MatchString(ref) {
		return ref
	}
	out, err := exec.Command("git", "ls-remote", repoURL, ref, "refs/heads/"+ref, "refs/tags/"+ref).Output()
	if err != nil {
		return ref
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 && shaRe.MatchString(fields[0]) {
			return fields[0]
		}
	}
	return ref
}

// serversFor lists servers hosting a component, optionally filtered.
func serversFor(cfg *config.Root, comp, serverFilter string) ([]string, error) {
	def, ok := cfg.Components[comp]
	if !ok {
		return nil, fmt.Errorf("unknown component %q (defined: %s)", comp, strings.Join(cfg.ComponentNames(), ", "))
	}
	if serverFilter == "" {
		return def.Servers, nil
	}
	for _, sv := range def.Servers {
		if sv == serverFilter {
			return []string{sv}, nil
		}
	}
	return nil, fmt.Errorf("component %q is not assigned to server %q (its servers: %s)", comp, serverFilter, strings.Join(def.Servers, ", "))
}

// StatusRow is one component's state on one server.
type StatusRow struct {
	Server    string                 `json:"server"`
	Component string                 `json:"component"`
	State     release.ComponentState `json:"state"`
}

// FetchStatus reads state.json from every server in parallel. Unreachable
// servers are reported by name rather than failing the whole call.
func FetchStatus(cfg *config.Root, serverFilter string) ([]StatusRow, []string, error) {
	if err := sshx.CheckBinaries(); err != nil {
		return nil, nil, err
	}
	names := cfg.ServerNames()
	if serverFilter != "" {
		if _, ok := cfg.Servers[serverFilter]; !ok {
			return nil, nil, fmt.Errorf("unknown server %q (defined: %s)", serverFilter, strings.Join(names, ", "))
		}
		names = []string{serverFilter}
	}
	var rows []StatusRow
	var unreachable []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, sv := range names {
		target := sshx.Target{Name: sv, Server: cfg.Servers[sv]}
		wg.Add(1)
		go func(sv string, target sshx.Target) {
			defer wg.Done()
			cmd := target.RemoteCmd([]string{target.Server.F1Bin(), "status", "--local", "--json", "--root", target.Server.Root})
			outStr, err := target.Output(cmd, io.Discard)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				unreachable = append(unreachable, sv)
				return
			}
			var st release.State
			if err := json.Unmarshal([]byte(outStr), &st); err != nil {
				unreachable = append(unreachable, sv)
				return
			}
			for comp, cs := range st.Components {
				rows = append(rows, StatusRow{sv, comp, cs})
			}
		}(sv, target)
	}
	wg.Wait()
	sort.Strings(unreachable)
	return sortedRows(rows, func(r StatusRow) string { return r.Server + "/" + r.Component }), unreachable, nil
}

// Status fetches state.json from every server and prints one merged table.
func Status(cfg *config.Root, serverFilter string) error {
	rows, unreachable, err := FetchStatus(cfg, serverFilter)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		ui.Printf("nothing deployed yet")
	} else {
		ui.Printf("%-14s %-16s %-8s %-28s %-9s %-11s %s", "SERVER", "COMPONENT", "STATUS", "RELEASE", "SHA", "SLOT", "DEPLOYED")
		for _, r := range rows {
			status := r.State.Status
			if status == "failed" {
				status = ui.Red(status)
			} else {
				status = ui.Green(status)
			}
			ui.Printf("%-14s %-16s %-8s %-28s %-9s %-11s %s", r.Server, r.Component, status, orDash(r.State.Release), release.ShortSha(r.State.Sha), slotLabel(r.State), release.Ago(r.State.DeployedAt))
		}
	}
	if len(unreachable) > 0 {
		return fmt.Errorf("could not read status from: %s (is f1 set up there? run `f1 server setup`)", strings.Join(unreachable, ", "))
	}
	return nil
}

// Logs streams a component's logs from its server(s).
func Logs(cfg *config.Root, comp, serverFilter string, lines int, follow bool) error {
	if err := sshx.CheckBinaries(); err != nil {
		return err
	}
	servers, err := serversFor(cfg, comp, serverFilter)
	if err != nil {
		return err
	}
	if follow && len(servers) > 1 {
		return fmt.Errorf("%q runs on %d servers — pick one with --server for -f", comp, len(servers))
	}
	for _, sv := range servers {
		target := sshx.Target{Name: sv, Server: cfg.Servers[sv]}
		out := ui.NewPrefix(sv)
		words := []string{target.Server.F1Bin(), "logs", comp, "--local", "--root", target.Server.Root, "-n", strconv.Itoa(lines)}
		if follow {
			words = append(words, "-f")
		}
		if err := target.Run(target.RemoteCmd(words), out, follow); err != nil {
			out.Flush()
			return err
		}
		out.Flush()
	}
	return nil
}

// Rollback rolls a component back on its server(s).
func Rollback(cfg *config.Root, comp, serverFilter string) error {
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
		words := []string{target.Server.F1Bin(), "rollback", comp, "--local", "--root", target.Server.Root}
		if err := target.Run(target.RemoteCmd(words), out, false); err != nil {
			out.Failf("rollback failed: %v", err)
			failed = append(failed, sv)
		}
		out.Flush()
	}
	if len(failed) > 0 {
		return fmt.Errorf("rollback failed on: %s", strings.Join(failed, ", "))
	}
	return nil
}

func sortedRows[T any](rows []T, key func(T) string) []T {
	sorted := append([]T(nil), rows...)
	for i := 1; i < len(sorted); i++ { // insertion sort; n is tiny
		for j := i; j > 0 && key(sorted[j]) < key(sorted[j-1]); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted
}
