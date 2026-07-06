// Local (on-server) implementations of status, logs, and rollback. The
// dev-side commands invoke these over SSH with --local.
package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/Alhasan-softwear/f1-runner/internal/config"
	"github.com/Alhasan-softwear/f1-runner/internal/gitx"
	"github.com/Alhasan-softwear/f1-runner/internal/release"
	"github.com/Alhasan-softwear/f1-runner/internal/runtime"
	"github.com/Alhasan-softwear/f1-runner/internal/ui"
)

// StatusLocal prints this server's deploy state, as JSON (for the dev-side
// aggregator) or a human table.
func StatusLocal(root string, asJSON bool) error {
	layout := release.Layout{Root: root}
	state, err := release.LoadState(layout)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(state)
	}
	if len(state.Components) == 0 {
		ui.Printf("nothing deployed yet (root %s)", root)
		return nil
	}
	ui.Printf("%-16s %-8s %-28s %-9s %s", "COMPONENT", "STATUS", "RELEASE", "SHA", "DEPLOYED")
	for _, name := range sortedKeys(state.Components) {
		c := state.Components[name]
		status := c.Status
		if status == "failed" {
			status = ui.Red(status)
		} else {
			status = ui.Green(status)
		}
		ui.Printf("%-16s %-8s %-28s %-9s %s", name, status, orDash(c.Release), release.ShortSha(c.Sha), release.Ago(c.DeployedAt))
	}
	return nil
}

// manifestFor loads a component's manifest from the deployed sha, so logs and
// rollback honor the manifest as of what is actually running.
func manifestFor(layout release.Layout, comp string) (*config.Manifest, *config.Root, *release.State, error) {
	state, err := release.LoadState(layout)
	if err != nil {
		return nil, nil, nil, err
	}
	cs, ok := state.Components[comp]
	if !ok || cs.Sha == "" {
		return nil, nil, nil, fmt.Errorf("component %q has never deployed successfully on this server", comp)
	}
	repo := gitx.At(layout.Root, "")
	rootRaw, err := repo.ShowFile(cs.Sha, "f1.yml")
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := config.ParseRoot(rootRaw)
	if err != nil {
		return nil, nil, nil, err
	}
	def, ok := cfg.Components[comp]
	if !ok {
		return nil, nil, nil, fmt.Errorf("component %q is missing from f1.yml at deployed commit %s", comp, release.ShortSha(cs.Sha))
	}
	raw, err := repo.ShowFile(cs.Sha, def.Path+"/f1.yml")
	if err != nil {
		return nil, nil, nil, err
	}
	m, err := config.ParseManifest(raw, comp)
	if err != nil {
		return nil, nil, nil, err
	}
	return m, cfg, state, nil
}

// LogsLocal tails a component's logs: compose logs for docker, the manifest's
// logs script or $F1_LOG for script runtimes. No timeout — follow mode runs
// until the client disconnects.
func LogsLocal(root, comp string, lines int, follow bool) error {
	layout := release.Layout{Root: root}
	m, _, _, err := manifestFor(layout, comp)
	if err != nil {
		return err
	}
	current := layout.Current(comp)
	if current == "" {
		return fmt.Errorf("component %q has no current release", comp)
	}
	var cmd *exec.Cmd
	switch {
	case m.Runtime == "docker":
		base, err := runtime.ComposeCommand()
		if err != nil {
			return err
		}
		args := append(base[1:], "-p", "f1-"+comp, "-f", m.Docker.Compose, "logs", "--tail", strconv.Itoa(lines))
		if follow {
			args = append(args, "-f")
		}
		cmd = exec.Command(base[0], args...)
	case m.Scripts.Logs != "":
		cmd = exec.Command("sh", "-c", m.Scripts.Logs)
	default:
		logFile := filepath.Join(layout.SharedDir(comp), "app.log")
		if _, err := os.Stat(logFile); err != nil {
			return fmt.Errorf("no logs: define scripts.logs in the manifest or have the app write to $F1_LOG (%s)", logFile)
		}
		args := []string{"-n", strconv.Itoa(lines)}
		if follow {
			args = append(args, "-f")
		}
		cmd = exec.Command("tail", append(args, logFile)...)
	}
	cmd.Dir = current
	cmd.Env = append(os.Environ(),
		"F1_ROOT="+root,
		"F1_COMPONENT="+comp,
		"F1_SHARED="+layout.SharedDir(comp),
		"F1_LOG="+filepath.Join(layout.SharedDir(comp), "app.log"),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RollbackLocal flips a component back to its previous release and restarts
// it. Instant: no fetch, no build.
func RollbackLocal(root, comp string) error {
	out := ui.NewBarePrefix()
	defer out.Flush()
	layout := release.Layout{Root: root}
	m, _, state, err := manifestFor(layout, comp)
	if err != nil {
		return err
	}
	cs := state.Components[comp]
	if cs.Previous == nil || cs.Previous.Release == "" {
		return fmt.Errorf("no previous release recorded for %q — nothing to roll back to", comp)
	}
	prevDir := filepath.Join(layout.ReleasesDir(comp), cs.Previous.Release)
	if _, err := os.Stat(prevDir); err != nil {
		return fmt.Errorf("previous release %s no longer exists on disk (pruned?)", cs.Previous.Release)
	}

	// The previous release ran under its own manifest — prefer it.
	if pm, _, _, err := manifestForSha(layout, comp, cs.Previous.Sha); err == nil {
		m = pm
	}

	env, err := stepEnv(layout, comp, cs.Previous.Sha, prevDir, m)
	if err != nil {
		return err
	}

	out.Step("%s: rolling back to %s (%s)", comp, cs.Previous.Release, release.ShortSha(cs.Previous.Sha))
	oldCurrent := layout.Current(comp)
	switch m.Runtime {
	case "docker":
		if err := runtime.Compose(comp, prevDir, m.Docker.Compose, env, out, "up", "-d", "--remove-orphans", "--build"); err != nil {
			return err
		}
		if err := layout.Flip(comp, prevDir); err != nil {
			return err
		}
	case "script":
		if oldCurrent != "" && m.Scripts.Stop != "" {
			oldEnv := replaceEnv(env, "F1_RELEASE", oldCurrent)
			if err := runtime.Exec(oldCurrent, m.Scripts.Stop, oldEnv, out); err != nil {
				out.Notef("%s: stop reported an error (continuing): %v", comp, err)
			}
		}
		if err := layout.Flip(comp, prevDir); err != nil {
			return err
		}
		if err := runtime.Exec(layout.CurrentLink(comp), m.Scripts.Start, env, out); err != nil {
			return err
		}
	}
	if m.Health.Defined() {
		out.Step("%s: health check", comp)
		if err := runtime.HealthCheck(m.Health, prevDir, env, out); err != nil {
			return err
		}
	}

	state.Components[comp] = release.ComponentState{
		Sha:        cs.Previous.Sha,
		Release:    cs.Previous.Release,
		Runtime:    m.Runtime,
		Status:     "ok",
		DeployedAt: time.Now().UTC().Format(time.RFC3339),
		Previous:   &release.Prev{Sha: cs.Sha, Release: cs.Release}, // rollback twice = toggle
	}
	if err := state.Save(layout); err != nil {
		return err
	}
	out.Okf("%s: rolled back to %s", comp, release.ShortSha(cs.Previous.Sha))
	return nil
}

func manifestForSha(layout release.Layout, comp, sha string) (*config.Manifest, *config.Root, *release.State, error) {
	repo := gitx.At(layout.Root, "")
	rootRaw, err := repo.ShowFile(sha, "f1.yml")
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := config.ParseRoot(rootRaw)
	if err != nil {
		return nil, nil, nil, err
	}
	def, ok := cfg.Components[comp]
	if !ok {
		return nil, nil, nil, fmt.Errorf("component %q missing at %s", comp, release.ShortSha(sha))
	}
	raw, err := repo.ShowFile(sha, def.Path+"/f1.yml")
	if err != nil {
		return nil, nil, nil, err
	}
	m, err := config.ParseManifest(raw, comp)
	return m, cfg, nil, err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
