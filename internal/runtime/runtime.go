// Package runtime executes component lifecycle steps on the server: shell
// scripts, docker compose operations, and health checks.
package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Alhasan-softwear/f1-runner/internal/config"
)

// StepTimeout bounds any single lifecycle command (setup/build/start/...).
const StepTimeout = 20 * time.Minute

// Exec runs a shell command in dir with extraEnv appended, streaming output.
func Exec(dir, command string, extraEnv []string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), StepTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = out
	cmd.Stderr = out
	setProcGroup(cmd)
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("step timed out after %s: %s", StepTimeout, command)
	}
	if err != nil {
		return fmt.Errorf("`%s` failed: %w", command, err)
	}
	return nil
}

// LoadEnvFile parses a KEY=VALUE file (comments and blank lines ignored,
// optional surrounding quotes stripped). A missing file is not an error —
// the manifest may declare an env_file the operator hasn't created yet.
func LoadEnvFile(path string) ([]string, bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var env []string
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.Index(line, "=")
		if eq <= 0 {
			return nil, true, fmt.Errorf("%s:%d: expected KEY=VALUE", path, i+1)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		env = append(env, key+"="+val)
	}
	return env, true, nil
}

// ComposeCommand returns the compose invocation available on this machine:
// modern plugin ("docker compose") or legacy binary ("docker-compose").
func ComposeCommand() ([]string, error) {
	if exec.Command("docker", "compose", "version").Run() == nil {
		return []string{"docker", "compose"}, nil
	}
	if _, err := exec.LookPath("docker-compose"); err == nil {
		return []string{"docker-compose"}, nil
	}
	return nil, fmt.Errorf("neither `docker compose` nor `docker-compose` is available")
}

// Compose runs a compose subcommand for a component's project from dir.
// Project name is pinned to f1-<comp> so a compose up from a new release dir
// replaces the containers started from the previous one.
func Compose(comp, dir, composeFile string, extraEnv []string, out io.Writer, args ...string) error {
	base, err := ComposeCommand()
	if err != nil {
		return err
	}
	full := append(append([]string{}, base[1:]...), "-p", "f1-"+comp, "-f", composeFile)
	full = append(full, args...)
	ctx, cancel := context.WithTimeout(context.Background(), StepTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, base[0], full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = out
	cmd.Stderr = out
	setProcGroup(cmd)
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("compose %s timed out after %s", strings.Join(args, " "), StepTimeout)
		}
		return fmt.Errorf("compose %s failed: %w", strings.Join(args, " "), err)
	}
	return nil
}

// HealthCheck runs the manifest's health probe with retries. Returns nil when
// the component reports healthy.
func HealthCheck(h config.Health, dir string, extraEnv []string, out io.Writer) error {
	retries := h.RetriesOrDefault()
	interval := h.IntervalOrDefault()
	var lastErr error
	for attempt := 1; attempt <= retries; attempt++ {
		if attempt > 1 {
			time.Sleep(interval)
		}
		lastErr = probe(h, dir, extraEnv, out)
		if lastErr == nil {
			return nil
		}
		fmt.Fprintf(out, "health attempt %d/%d: %v\n", attempt, retries, lastErr)
	}
	return fmt.Errorf("unhealthy after %d attempts: %w", retries, lastErr)
}

func probe(h config.Health, dir string, extraEnv []string, out io.Writer) error {
	if h.URL != "" {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(h.URL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("GET %s -> %s", h.URL, resp.Status)
		}
		return nil
	}
	// h.Cmd — run quietly; only health output on failure would be noise.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", h.Cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	setProcGroup(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("`%s` failed", h.Cmd)
	}
	return nil
}
