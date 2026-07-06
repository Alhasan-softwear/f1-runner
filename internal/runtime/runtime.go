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
	gorun "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Alhasan-softwear/f1-runner/internal/config"
)

// StepTimeout bounds any single lifecycle command (setup/build/start/...).
const StepTimeout = 20 * time.Minute

// shellArgv maps a manifest shell to an interpreter invocation. Empty picks
// the platform default (sh on unix, cmd on windows).
func shellArgv(shell string) []string {
	switch shell {
	case "bash":
		return []string{"bash", "-c"}
	case "cmd":
		return []string{"cmd", "/c"}
	case "powershell":
		return []string{"powershell", "-NoProfile", "-Command"}
	case "sh":
		return []string{"sh", "-c"}
	default:
		if gorun.GOOS == "windows" {
			return []string{"cmd", "/c"}
		}
		return []string{"sh", "-c"}
	}
}

// Exec runs a shell command in dir with extraEnv appended, streaming output.
func Exec(dir, command, shell string, extraEnv []string, out io.Writer) error {
	argv := shellArgv(shell)
	ctx, cancel := context.WithTimeout(context.Background(), StepTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], command)...)
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

// Compose runs a compose subcommand for the given project from dir. Pinning
// the project name means an `up` from a new release dir replaces the
// containers started from the previous one (and blue/green slots get their
// own projects).
func Compose(project, dir, composeFile string, extraEnv []string, out io.Writer, args ...string) error {
	base, err := ComposeCommand()
	if err != nil {
		return err
	}
	full := append(append([]string{}, base[1:]...), "-p", project, "-f", composeFile)
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

// SubstPort replaces $F1_PORT / ${F1_PORT} in a string (used for health URLs
// under blue/green, where the live port alternates).
func SubstPort(s string, port int) string {
	if port <= 0 {
		return s
	}
	p := strconv.Itoa(port)
	s = strings.ReplaceAll(s, "${F1_PORT}", p)
	return strings.ReplaceAll(s, "$F1_PORT", p)
}

// HealthCheck runs the manifest's health probe with retries. port > 0
// substitutes $F1_PORT in a health URL. Returns nil when healthy.
func HealthCheck(h config.Health, dir, shell string, port int, extraEnv []string, out io.Writer) error {
	retries := h.RetriesOrDefault()
	interval := h.IntervalOrDefault()
	var lastErr error
	for attempt := 1; attempt <= retries; attempt++ {
		if attempt > 1 {
			time.Sleep(interval)
		}
		lastErr = probe(h, dir, shell, port, extraEnv)
		if lastErr == nil {
			return nil
		}
		fmt.Fprintf(out, "health attempt %d/%d: %v\n", attempt, retries, lastErr)
	}
	return fmt.Errorf("unhealthy after %d attempts: %w", retries, lastErr)
}

func probe(h config.Health, dir, shell string, port int, extraEnv []string) error {
	if h.URL != "" {
		url := SubstPort(h.URL, port)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("GET %s -> %s", url, resp.Status)
		}
		return nil
	}
	argv := shellArgv(shell)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], h.Cmd)...)
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
