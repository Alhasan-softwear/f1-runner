//go:build windows

package runtime

import "os/exec"

// Servers are Linux; on Windows (dev machine) apply isn't supported, so the
// default context cancel behavior is fine.
func setProcGroup(cmd *exec.Cmd) {}
