// Package release owns the on-server directory layout and deploy state:
//
//	/opt/f1/
//	  bin/f1                 the runner binary itself
//	  repo/                  bare clone of the monorepo
//	  deploy_key(.pub)       optional git deploy key
//	  env/<comp>.env         user-managed env files, never in the repo
//	  state.json             what is deployed where
//	  apps/<comp>/
//	    releases/<stamp>/    one dir per deploy
//	    current -> releases/<stamp>
//	    shared/              persists across releases ($F1_SHARED)
package release

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Layout struct {
	Root string // e.g. /opt/f1
}

func (l Layout) AppDir(comp string) string      { return filepath.Join(l.Root, "apps", comp) }
func (l Layout) ReleasesDir(comp string) string { return filepath.Join(l.AppDir(comp), "releases") }
func (l Layout) CurrentLink(comp string) string { return filepath.Join(l.AppDir(comp), "current") }
func (l Layout) SharedDir(comp string) string   { return filepath.Join(l.AppDir(comp), "shared") }
func (l Layout) StatePath() string              { return filepath.Join(l.Root, "state.json") }

// NewReleaseDir creates releases/<utc-stamp>-<shortsha> and the shared dir.
func (l Layout) NewReleaseDir(comp, sha string, now time.Time) (string, error) {
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	name := now.UTC().Format("20060102-150405") + "-" + short
	dir := filepath.Join(l.ReleasesDir(comp), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(l.SharedDir(comp), 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Current resolves the current symlink to a release dir ("" if none).
func (l Layout) Current(comp string) string {
	target, err := os.Readlink(l.CurrentLink(comp))
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(l.CurrentLink(comp)), target)
	}
	return filepath.Clean(target)
}

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

// Prune deletes old release dirs, keeping the newest `keep` plus whatever
// current points at.
func (l Layout) Prune(comp string, keep int) []string {
	entries, err := os.ReadDir(l.ReleasesDir(comp))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // stamp prefix sorts chronologically
	if len(names) <= keep {
		return nil
	}
	current := filepath.Base(l.Current(comp))
	var removed []string
	for _, name := range names[:len(names)-keep] {
		if name == current {
			continue
		}
		if err := os.RemoveAll(filepath.Join(l.ReleasesDir(comp), name)); err == nil {
			removed = append(removed, name)
		}
	}
	return removed
}

// ComponentState records one component's deploy status on this server.
type ComponentState struct {
	Sha        string `json:"sha"`
	Release    string `json:"release"` // dir name under releases/
	Runtime    string `json:"runtime"`
	Status     string `json:"status"` // ok | failed
	DeployedAt string `json:"deployedAt"`
	Previous   *Prev  `json:"previous,omitempty"`
	Error      string `json:"error,omitempty"`
}

type Prev struct {
	Sha     string `json:"sha"`
	Release string `json:"release"`
}

type State struct {
	Components map[string]ComponentState `json:"components"`
}

func LoadState(l Layout) (*State, error) {
	raw, err := os.ReadFile(l.StatePath())
	if os.IsNotExist(err) {
		return &State{Components: map[string]ComponentState{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("state.json is corrupt (%w) — fix or delete it", err)
	}
	if s.Components == nil {
		s.Components = map[string]ComponentState{}
	}
	return &s, nil
}

// Save writes state atomically (temp file + rename).
func (s *State) Save(l Layout) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.Root, 0o755); err != nil {
		return err
	}
	tmp := l.StatePath() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.StatePath())
}

// Ago renders a deployedAt timestamp as a friendly age ("2h ago").
func Ago(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ShortSha trims a sha for display.
func ShortSha(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "-"
	}
	return sha
}

// TrimName keeps table cells sane.
func TrimName(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
