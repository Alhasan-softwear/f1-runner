// Package agent runs f1 on a server as a small HTTP daemon so deploys can be
// triggered by CI pipelines and git-host webhooks instead of (or alongside)
// SSH pushes from a dev machine.
//
//	f1 agent --root /opt/f1 --repo git@github.com:me/app.git --token SECRET
//
// Endpoints:
//
//	POST /deploy   — Bearer token or GitHub X-Hub-Signature-256 (secret = token).
//	                 Accepts a GitHub push event (deploys when the pushed branch
//	                 matches --branch, at the pushed sha) or a plain JSON body:
//	                 {"components": ["web"], "ref": "main", "force": false}.
//	GET  /status   — this server's state.json
//	GET  /healthz  — liveness
//
// Deploys are serialized: concurrent triggers queue behind a mutex. Each
// deploy re-executes this binary (`f1 apply …`) so its output can stream back
// to the caller and a crashing deploy can never take the agent down.
package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Root       string
	RepoURL    string
	Branch     string // deploy trigger branch + default ref, e.g. "main"
	Listen     string // e.g. ":9123"
	Token      string
	Components []string // restrict what this agent may deploy (empty = all)
}

type server struct {
	opts Options
	mu   sync.Mutex // one deploy at a time
}

func Run(opts Options) error {
	if opts.Token == "" {
		return fmt.Errorf("an auth token is required: pass --token or set F1_AGENT_TOKEN")
	}
	if opts.RepoURL == "" {
		return fmt.Errorf("--repo is required")
	}
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	if opts.Listen == "" {
		opts.Listen = ":9123"
	}
	s := &server{opts: opts}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/deploy", s.handleDeploy)
	httpServer := &http.Server{
		Addr:              opts.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Printf("f1 agent listening on %s (repo %s, branch %s)\n", opts.Listen, opts.RepoURL, opts.Branch)
	return httpServer.ListenAndServe()
}

// authorized accepts either a Bearer token or a GitHub webhook HMAC signature
// computed with the token as the shared secret.
func (s *server) authorized(r *http.Request, body []byte) bool {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		given := strings.TrimPrefix(auth, "Bearer ")
		return subtle.ConstantTimeCompare([]byte(given), []byte(s.opts.Token)) == 1
	}
	if sig := r.Header.Get("X-Hub-Signature-256"); strings.HasPrefix(sig, "sha256=") {
		mac := hmac.New(sha256.New, []byte(s.opts.Token))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(sig), []byte(want))
	}
	// token as query param, for CI systems that can't set headers
	if tok := r.URL.Query().Get("token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(s.opts.Token)) == 1
	}
	return false
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r, nil) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	raw, err := os.ReadFile(s.opts.Root + "/state.json")
	if os.IsNotExist(err) {
		raw = []byte(`{"components":{}}`)
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}

type deployRequest struct {
	Components []string `json:"components"`
	Ref        string   `json:"ref"`
	Force      bool     `json:"force"`
	// GitHub push event fields
	GitRef string `json:"-"`
	After  string `json:"after"`
}

func (s *server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !s.authorized(r, body) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	req := deployRequest{Ref: s.opts.Branch}
	if len(body) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			http.Error(w, "body must be JSON", http.StatusBadRequest)
			return
		}
		json.Unmarshal(body, &req)
		if ghRef, ok := raw["ref"]; ok {
			var refStr string
			if json.Unmarshal(ghRef, &refStr) == nil && strings.HasPrefix(refStr, "refs/") {
				// GitHub push event: only deploy pushes to the configured branch.
				if refStr != "refs/heads/"+s.opts.Branch {
					w.WriteHeader(http.StatusOK)
					fmt.Fprintf(w, "ignored: push to %s (agent deploys %s)\n", refStr, s.opts.Branch)
					return
				}
				req.Ref = s.opts.Branch
				if req.After != "" && req.After != strings.Repeat("0", 40) {
					req.Ref = req.After
				}
			} else if json.Unmarshal(ghRef, &refStr) == nil && refStr != "" {
				req.Ref = refStr
			}
		}
	}
	comps := req.Components
	if len(comps) == 0 {
		comps = s.opts.Components
	}

	// GitHub gives webhooks ~10s: acknowledge fast, deploy in the background.
	// CI callers who want the log can pass ?wait=1 and stream it.
	wait := r.URL.Query().Get("wait") == "1"
	if !wait {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "deploy of %s queued (ref %s)\n", compLabel(comps), req.Ref)
		go s.runDeploy(os.Stdout, comps, req.Ref, req.Force)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.runDeploy(&flushWriter{w: w}, comps, req.Ref, req.Force)
}

func compLabel(comps []string) string {
	if len(comps) == 0 {
		return "all components"
	}
	return strings.Join(comps, ",")
}

// runDeploy serializes deploys and shells out to this same binary.
func (s *server) runDeploy(out io.Writer, comps []string, ref string, force bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(out, "agent error: %v\n", err)
		return
	}
	args := []string{"apply", "--root", s.opts.Root, "--repo", s.opts.RepoURL, "--ref", ref}
	if len(comps) > 0 {
		args = append(args, "--components", strings.Join(comps, ","))
	}
	if force {
		args = append(args, "--force")
	}
	fmt.Fprintf(out, "== f1 %s\n", strings.Join(args, " "))
	cmd := exec.Command(self, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(out, "deploy failed: %v\n", err)
		return
	}
	fmt.Fprintln(out, "deploy ok")
}

type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
