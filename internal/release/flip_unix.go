//go:build !windows

package release

import (
	"fmt"
	"os"
	"path/filepath"
)

// Flip atomically points current at releaseDir (symlink swap via rename).
func (l Layout) Flip(comp, releaseDir string) error {
	link := l.CurrentLink(comp)
	tmp := link + ".tmp"
	os.Remove(tmp)
	// Relative target keeps the tree relocatable.
	rel, err := filepath.Rel(filepath.Dir(link), releaseDir)
	if err != nil {
		rel = releaseDir
	}
	if err := os.Symlink(rel, tmp); err != nil {
		return fmt.Errorf("symlink: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("activating release: %w", err)
	}
	return nil
}
