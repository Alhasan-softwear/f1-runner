//go:build windows

package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Flip points current at releaseDir. Windows (experimental): plain symlinks
// need admin or Developer Mode, so fall back to an NTFS junction, which any
// user may create. The swap is remove+recreate — a very short non-atomic
// window, acceptable for the experimental Windows server mode.
func (l Layout) Flip(comp, releaseDir string) error {
	link := l.CurrentLink(comp)
	abs, err := filepath.Abs(releaseDir)
	if err != nil {
		abs = releaseDir
	}
	tmp := link + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(abs, tmp); err == nil {
		os.Remove(link) // rename-over-existing fails on windows
		if err := os.Rename(tmp, link); err == nil {
			return nil
		}
		os.Remove(tmp)
	}
	// Junction fallback: mklink /J needs cmd, absolute backslash paths.
	os.Remove(link)
	out, err := exec.Command("cmd", "/c", "mklink", "/J",
		filepath.FromSlash(link), filepath.FromSlash(abs)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("activating release (mklink /J): %s", string(out))
	}
	return nil
}
