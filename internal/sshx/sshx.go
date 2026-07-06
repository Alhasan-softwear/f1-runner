// Package sshx runs commands on servers through the system ssh/scp binaries.
// Shelling out (rather than a Go SSH library) means the user's ~/.ssh/config,
// agent, passphrases, and ProxyJump setups all work unchanged.
package sshx

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Alhasan-softwear/f1-runner/internal/config"
)

// Target identifies one server from the root config.
type Target struct {
	Name   string
	Server config.Server
}

func (t Target) addr() string { return t.Server.User + "@" + t.Server.Host }

func (t Target) baseArgs() []string {
	args := []string{"-p", strconv.Itoa(t.Server.Port)}
	if t.Server.Key != "" {
		args = append(args, "-i", expandHome(t.Server.Key))
	}
	// Deploys are non-interactive; fail fast instead of hanging on a prompt.
	args = append(args, "-o", "BatchMode=yes")
	args = append(args, t.Server.SSHOpts...)
	return args
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + strings.TrimPrefix(p, "~")
		}
	}
	return p
}

// Quote makes a string safe as a single word for a remote POSIX shell.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:=@,", r) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteCmd quotes each word and joins them into one remote command string.
func QuoteCmd(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = Quote(w)
	}
	return strings.Join(quoted, " ")
}

// Run executes remoteCmd on the target, streaming combined output to out.
// stdin is connected when interactive is true (for -f log follows, Ctrl-C).
func (t Target) Run(remoteCmd string, out io.Writer, interactive bool) error {
	args := append(t.baseArgs(), t.addr(), remoteCmd)
	cmd := exec.Command("ssh", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if interactive {
		cmd.Stdin = os.Stdin
	}
	return cmd.Run()
}

// Output executes remoteCmd and returns its stdout (stderr goes to errOut).
func (t Target) Output(remoteCmd string, errOut io.Writer) (string, error) {
	args := append(t.baseArgs(), t.addr(), remoteCmd)
	cmd := exec.Command("ssh", args...)
	var sb strings.Builder
	cmd.Stdout = &sb
	cmd.Stderr = errOut
	err := cmd.Run()
	return sb.String(), err
}

// Upload copies a local file to remotePath on the target via scp.
func (t Target) Upload(localPath, remotePath string, out io.Writer) error {
	args := []string{"-P", strconv.Itoa(t.Server.Port)}
	if t.Server.Key != "" {
		args = append(args, "-i", expandHome(t.Server.Key))
	}
	args = append(args, "-o", "BatchMode=yes")
	args = append(args, t.Server.SSHOpts...)
	args = append(args, localPath, t.addr()+":"+remotePath)
	cmd := exec.Command("scp", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// CheckBinaries verifies ssh and scp exist on PATH before we try anything.
func CheckBinaries() error {
	for _, bin := range []string{"ssh", "scp"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%q not found on PATH — install OpenSSH client (on Windows it ships with Git and with the optional OpenSSH feature)", bin)
		}
	}
	return nil
}
