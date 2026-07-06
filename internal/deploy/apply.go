// Server-side orchestration: f1 apply materializes and activates releases on
// the machine it runs on. The dev-side `f1 deploy` invokes this over SSH.
package deploy

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Alhasan-softwear/f1-runner/internal/config"
	"github.com/Alhasan-softwear/f1-runner/internal/gitx"
	"github.com/Alhasan-softwear/f1-runner/internal/provision"
	"github.com/Alhasan-softwear/f1-runner/internal/release"
	"github.com/Alhasan-softwear/f1-runner/internal/runtime"
	"github.com/Alhasan-softwear/f1-runner/internal/ui"
)

type ApplyOptions struct {
	Root       string // f1 root on this machine, e.g. /opt/f1
	RepoURL    string
	Ref        string
	Components []string // empty = all components assigned in the root config
	Force      bool
	DryRun     bool
}

// Apply fetches the repo, decides which components need work, and deploys
// them in dependency order. Returns an error if any component failed.
func Apply(opts ApplyOptions) error {
	out := ui.NewBarePrefix()
	defer out.Flush()

	layout := release.Layout{Root: opts.Root}
	repo := gitx.At(opts.Root, opts.RepoURL)

	out.Step("fetch %s", opts.RepoURL)
	if err := repo.EnsureAndFetch(out); err != nil {
		return err
	}
	sha, err := repo.Resolve(opts.Ref)
	if err != nil {
		return err
	}
	out.Notef("ref %s = %s", opts.Ref, release.ShortSha(sha))

	rootRaw, err := repo.ShowFile(sha, "f1.yml")
	if err != nil {
		return fmt.Errorf("no f1.yml at the repo root in %s — run `f1 init` in the monorepo and commit it", release.ShortSha(sha))
	}
	cfg, err := config.ParseRoot(rootRaw)
	if err != nil {
		return err
	}

	targets := opts.Components
	if len(targets) == 0 {
		targets = cfg.ComponentNames()
	}
	for _, name := range targets {
		if _, ok := cfg.Components[name]; !ok {
			return fmt.Errorf("unknown component %q (this commit defines: %s)", name, strings.Join(cfg.ComponentNames(), ", "))
		}
	}
	waves, err := cfg.Waves(targets)
	if err != nil {
		return err
	}

	state, err := release.LoadState(layout)
	if err != nil {
		return err
	}

	var failed []string
	for wi, wave := range waves {
		if len(waves) > 1 && len(failed) == 0 {
			out.Notef("wave %d/%d: %s", wi+1, len(waves), strings.Join(wave, ", "))
		}
		if len(failed) > 0 {
			out.Notef("skipping wave %d (%s) — earlier failures", wi+1, strings.Join(wave, ", "))
			continue
		}
		for _, name := range wave {
			if err := deployOne(layout, repo, cfg, state, name, sha, opts, out); err != nil {
				out.Failf("%s: %v", name, err)
				failed = append(failed, name)
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

func deployOne(layout release.Layout, repo *gitx.Repo, cfg *config.Root, state *release.State, name, sha string, opts ApplyOptions, out *ui.PrefixWriter) error {
	comp := cfg.Components[name]
	prev := state.Components[name]

	if !opts.Force && prev.Status == "ok" {
		if prev.Sha == sha {
			out.Okf("%s: already at %s — skipping (use --force to redeploy)", name, release.ShortSha(sha))
			return nil
		}
		if !repo.ChangedIn(prev.Sha, sha, comp.Path) {
			out.Okf("%s: no changes in %s since %s — skipping (use --force)", name, comp.Path, release.ShortSha(prev.Sha))
			return nil
		}
	}

	manifestRaw, err := repo.ShowFile(sha, comp.ManifestPath())
	if err != nil {
		return err
	}
	m, err := config.ParseManifest(manifestRaw, name)
	if err != nil {
		return err
	}

	if opts.DryRun {
		out.Step("%s: would deploy %s (%s runtime) from %s", name, release.ShortSha(sha), m.Runtime, comp.Path)
		return nil
	}

	if len(m.Provision) > 0 {
		out.Step("%s: ensuring dependencies (%s)", name, strings.Join(m.Provision, ", "))
		if err := provision.Ensure(m.Provision, out); err != nil {
			return err
		}
	}

	out.Step("%s: deploying %s (%s runtime)", name, release.ShortSha(sha), m.Runtime)
	releaseDir, err := layout.NewReleaseDir(name, sha, time.Now())
	if err != nil {
		return err
	}
	if err := repo.ArchiveInto(sha, comp.Path, releaseDir); err != nil {
		return err
	}

	env, err := stepEnv(layout, name, sha, releaseDir, m)
	if err != nil {
		return err
	}

	oldCurrent := layout.Current(name)
	var deployErr error
	slot, port := "", 0
	switch m.Runtime {
	case "docker":
		slot, port, deployErr = deployDocker(layout, name, m, releaseDir, oldCurrent, prev, env, out)
	case "script":
		deployErr = deployScript(layout, name, m, releaseDir, oldCurrent, env, out)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	entry := release.ComponentState{
		Sha:        sha,
		Release:    filepath.Base(releaseDir),
		Runtime:    m.Runtime,
		DeployedAt: now,
		Slot:       slot,
		Port:       port,
	}
	if prev.Sha != "" && prev.Status == "ok" {
		entry.Previous = &release.Prev{Sha: prev.Sha, Release: prev.Release}
	} else if prev.Previous != nil {
		entry.Previous = prev.Previous // keep the last good one across failed tries
	}
	if deployErr != nil {
		// The failed release dir is kept for debugging; current still points
		// at the old release (or was restored to it).
		entry.Status = "failed"
		entry.Error = deployErr.Error()
		entry.Release = prev.Release
		entry.Sha = prev.Sha
		entry.Slot = prev.Slot
		entry.Port = prev.Port
		if prev.Status != "ok" {
			entry.Release = ""
			entry.Sha = ""
		}
		state.Components[name] = entry
		state.Save(layout)
		return deployErr
	}
	entry.Status = "ok"
	state.Components[name] = entry
	if err := state.Save(layout); err != nil {
		return err
	}
	if removed := layout.Prune(name, m.Keep); len(removed) > 0 {
		out.Notef("%s: pruned %d old release(s)", name, len(removed))
	}
	out.Okf("%s: live at %s (release %s)", name, release.ShortSha(sha), filepath.Base(releaseDir))
	return nil
}

// stepEnv builds the environment every lifecycle command runs with. The
// component's env file (explicit env_file, or the default
// <root>/env/<comp>.env managed by `f1 env set`) is appended.
func stepEnv(layout release.Layout, name, sha, releaseDir string, m *config.Manifest) ([]string, error) {
	env := []string{
		"F1_ROOT=" + layout.Root,
		"F1_COMPONENT=" + name,
		"F1_RELEASE=" + releaseDir,
		"F1_CURRENT=" + layout.CurrentLink(name),
		"F1_SHARED=" + layout.SharedDir(name),
		"F1_LOG=" + filepath.Join(layout.SharedDir(name), "app.log"),
		"F1_REF=" + sha,
	}
	path, required := m.EnvFile, true
	if path == "" {
		path, required = layout.EnvFile(name), false
	}
	fileEnv, found, err := runtime.LoadEnvFile(path)
	if err != nil {
		return nil, err
	}
	if !found && required {
		return nil, fmt.Errorf("env_file %s does not exist on this server — create it with `f1 env set %s KEY=VALUE` (or remove env_file from the manifest)", path, name)
	}
	return append(env, fileEnv...), nil
}

// composeProject names the compose project: one per component, or one per
// slot under blue/green.
func composeProject(name, slot string) string {
	if slot == "" {
		return "f1-" + name
	}
	return "f1-" + name + "-" + slot
}

func deployDocker(layout release.Layout, name string, m *config.Manifest, releaseDir, oldCurrent string, prev release.ComponentState, env []string, out *ui.PrefixWriter) (string, int, error) {
	slot, port := "", 0
	if bg := m.BlueGreen; bg != nil {
		// Alternate slots; first deploy lands on blue.
		idx := 0
		if prev.Status == "ok" && prev.Slot == config.SlotNames[0] {
			idx = 1
		}
		slot, port = config.SlotNames[idx], bg.Ports[idx]
		env = append(env, bg.EnvVar()+"="+strconv.Itoa(port), "F1_SLOT="+slot)
		if bg.EnvVar() != "F1_PORT" {
			env = append(env, "F1_PORT="+strconv.Itoa(port))
		}
		out.Notef("%s: blue/green — deploying to %s slot on port %d", name, slot, port)
	}
	project := composeProject(name, slot)

	if m.Scripts.Setup != "" {
		out.Step("%s: setup", name)
		if err := runtime.Exec(releaseDir, m.Scripts.Setup, m.Shell, env, out); err != nil {
			return slot, port, err
		}
	}
	out.Step("%s: docker compose build", name)
	if err := runtime.Compose(project, releaseDir, m.Docker.Compose, env, out, "build"); err != nil {
		return slot, port, err
	}
	out.Step("%s: docker compose up -d", name)
	if err := runtime.Compose(project, releaseDir, m.Docker.Compose, env, out, "up", "-d", "--remove-orphans"); err != nil {
		return slot, port, err
	}
	if m.Health.Defined() {
		out.Step("%s: health check", name)
		if err := runtime.HealthCheck(m.Health, releaseDir, m.Shell, port, env, out); err != nil {
			if m.BlueGreen != nil {
				// The old slot never stopped serving; just tear the new one down.
				out.Notef("%s: tearing down unhealthy %s slot (old slot still live)", name, slot)
				runtime.Compose(project, releaseDir, m.Docker.Compose, env, out, "down", "--remove-orphans")
			} else if oldCurrent != "" {
				out.Notef("%s: restoring previous release", name)
				if rerr := runtime.Compose(project, oldCurrent, m.Docker.Compose, env, out, "up", "-d", "--remove-orphans"); rerr != nil {
					out.Failf("%s: restore also failed: %v", name, rerr)
				}
			}
			return slot, port, err
		}
	}
	if bg := m.BlueGreen; bg != nil {
		if bg.Switch != "" {
			out.Step("%s: switch traffic -> %s (:%d)", name, slot, port)
			switchEnv := env
			if prev.Port > 0 {
				switchEnv = append(switchEnv, "F1_OLD_PORT="+strconv.Itoa(prev.Port))
			}
			if err := runtime.Exec(releaseDir, bg.Switch, m.Shell, switchEnv, out); err != nil {
				out.Notef("%s: tearing down %s slot after failed switch", name, slot)
				runtime.Compose(project, releaseDir, m.Docker.Compose, env, out, "down", "--remove-orphans")
				return slot, port, err
			}
		}
		if prev.Slot != "" && prev.Slot != slot && oldCurrent != "" {
			out.Step("%s: stopping old %s slot", name, prev.Slot)
			if err := runtime.Compose(composeProject(name, prev.Slot), oldCurrent, m.Docker.Compose, env, out, "down", "--remove-orphans"); err != nil {
				out.Notef("%s: could not stop old slot (continuing): %v", name, err)
			}
		}
	}
	return slot, port, layout.Flip(name, releaseDir)
}

func deployScript(layout release.Layout, name string, m *config.Manifest, releaseDir, oldCurrent string, env []string, out *ui.PrefixWriter) error {
	if m.Scripts.Setup != "" {
		out.Step("%s: setup", name)
		if err := runtime.Exec(releaseDir, m.Scripts.Setup, m.Shell, env, out); err != nil {
			return err
		}
	}
	if m.Scripts.Build != "" {
		out.Step("%s: build", name)
		if err := runtime.Exec(releaseDir, m.Scripts.Build, m.Shell, env, out); err != nil {
			return err
		}
	}
	if oldCurrent != "" && m.Scripts.Stop != "" {
		out.Step("%s: stop old release", name)
		oldEnv := replaceEnv(env, "F1_RELEASE", oldCurrent)
		if err := runtime.Exec(oldCurrent, m.Scripts.Stop, m.Shell, oldEnv, out); err != nil {
			out.Notef("%s: stop reported an error (continuing): %v", name, err)
		}
	}
	if err := layout.Flip(name, releaseDir); err != nil {
		return err
	}
	out.Step("%s: start", name)
	startErr := runtime.Exec(layout.CurrentLink(name), m.Scripts.Start, m.Shell, env, out)
	if startErr == nil && m.Health.Defined() {
		out.Step("%s: health check", name)
		startErr = runtime.HealthCheck(m.Health, layout.CurrentLink(name), m.Shell, 0, env, out)
	}
	if startErr != nil {
		if oldCurrent != "" {
			out.Notef("%s: rolling back to previous release", name)
			if err := layout.Flip(name, oldCurrent); err == nil {
				oldEnv := replaceEnv(env, "F1_RELEASE", oldCurrent)
				if rerr := runtime.Exec(layout.CurrentLink(name), m.Scripts.Start, m.Shell, oldEnv, out); rerr != nil {
					out.Failf("%s: restart of previous release also failed: %v", name, rerr)
				}
			}
		}
		return startErr
	}
	return nil
}

func replaceEnv(env []string, key, val string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return append(out, key+"="+val)
}
