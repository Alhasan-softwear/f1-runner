// Package gitx maintains the server-side bare clone and materializes
// component subtrees from it. Everything shells out to the system git.
package gitx

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is a bare clone at <f1 root>/repo, fetched from URL with the deploy
// key at <f1 root>/deploy_key when present.
type Repo struct {
	Dir       string // bare repo directory
	URL       string
	DeployKey string // optional identity file for git-over-ssh
}

func At(f1Root, url string) *Repo {
	r := &Repo{Dir: filepath.Join(f1Root, "repo"), URL: url}
	if key := filepath.Join(f1Root, "deploy_key"); fileExists(key) {
		r.DeployKey = key
	}
	return r
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func (r *Repo) env() []string {
	env := os.Environ()
	if r.DeployKey != "" {
		env = append(env, fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o BatchMode=yes", r.DeployKey))
	}
	return env
}

// safeArgs prepends config overrides so f1 works regardless of the host's
// git hardening: bare-repo access (safe.bareRepository=explicit) and
// mixed-ownership roots (safe.directory) would otherwise break the managed
// clone under <root>/repo.
func safeArgs(args ...string) []string {
	return append([]string{"-c", "safe.bareRepository=all", "-c", "safe.directory=*"}, args...)
}

func (r *Repo) git(out io.Writer, args ...string) error {
	cmd := exec.Command("git", safeArgs(args...)...)
	cmd.Env = r.env()
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

func (r *Repo) gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", safeArgs(args...)...)
	cmd.Env = r.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// EnsureAndFetch clones the bare repo on first use, then fetches all branches
// and tags so any ref (branch, tag, or sha on a branch) is resolvable.
func (r *Repo) EnsureAndFetch(out io.Writer) error {
	if !fileExists(filepath.Join(r.Dir, "HEAD")) {
		if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
			return err
		}
		fmt.Fprintf(out, "cloning %s\n", r.URL)
		if err := r.git(out, "clone", "--bare", r.URL, r.Dir); err != nil {
			return fmt.Errorf("git clone --bare %s failed (is the deploy key added to the repo host?)", r.URL)
		}
		return nil
	}
	if err := r.git(out, "-C", r.Dir, "fetch", "--prune", "--tags", "--force", r.URL, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return fmt.Errorf("git fetch from %s failed", r.URL)
	}
	return nil
}

// Resolve turns a branch name, tag, or sha into a full commit sha.
func (r *Repo) Resolve(ref string) (string, error) {
	for _, candidate := range []string{"refs/remotes/origin/" + ref, "refs/heads/" + ref, ref + "^{commit}"} {
		if sha, err := r.gitOutput("-C", r.Dir, "rev-parse", "--verify", "--quiet", candidate); err == nil && sha != "" {
			return sha, nil
		}
	}
	return "", fmt.Errorf("ref %q not found in the repo (after fetch)", ref)
}

// ChangedIn reports whether anything under path differs between two commits.
// A missing/invalid oldSha (e.g. history rewrite) counts as changed.
func (r *Repo) ChangedIn(oldSha, newSha, path string) bool {
	if oldSha == "" || oldSha == newSha {
		return oldSha == ""
	}
	out, err := r.gitOutput("-C", r.Dir, "diff", "--name-only", oldSha, newSha, "--", path)
	if err != nil {
		return true
	}
	return out != ""
}

// ShowFile returns a file's content at a commit, e.g. ShowFile(sha, "apps/web/f1.yml").
func (r *Repo) ShowFile(sha, path string) ([]byte, error) {
	cmd := exec.Command("git", safeArgs("-C", r.Dir, "show", sha+":"+path)...)
	cmd.Env = r.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s not found at %s: %s", path, short(sha), strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// ArchiveInto materializes the subtree at path (commit sha) into destDir,
// stripping the path prefix — destDir becomes the component's release dir.
// The tar stream is read in-process, so the server needs no tar binary and
// prefix stripping is exact.
func (r *Repo) ArchiveInto(sha, path, destDir string) error {
	cmd := exec.Command("git", safeArgs("-C", r.Dir, "archive", "--format=tar", sha, "--", path)...)
	cmd.Env = r.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := extractTar(stdout, path, destDir)
	if extractErr != nil {
		// Don't Wait on a writer nobody is reading — it would deadlock.
		cmd.Process.Kill()
	} else {
		// Drain trailing record padding past the tar end marker; otherwise
		// git blocks writing it (small pipe buffers) and Wait hangs.
		io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	if extractErr != nil {
		return fmt.Errorf("extracting %s at %s: %w", path, short(sha), extractErr)
	}
	if waitErr != nil {
		return fmt.Errorf("git archive %s at %s: %s", path, short(sha), strings.TrimSpace(stderr.String()))
	}
	return nil
}

func extractTar(src io.Reader, stripPrefix, destDir string) error {
	prefix := strings.TrimSuffix(stripPrefix, "/") + "/"
	tr := tar.NewReader(src)
	seen := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := hdr.Name
		if name == stripPrefix || name == prefix { // the component dir entry itself
			seen = true
			continue
		}
		if !strings.HasPrefix(name, prefix) {
			continue // pax headers etc.
		}
		seen = true
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" {
			continue
		}
		// Defense against hostile archive entries.
		clean := filepath.Clean(rel)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("archive entry escapes release dir: %s", name)
		}
		target := filepath.Join(destDir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
	if !seen {
		return fmt.Errorf("path %q produced an empty archive — does it exist at that commit?", stripPrefix)
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
