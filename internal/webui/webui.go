// Package webui serves the f1 dashboard: a Coolify-style control panel for
// the monorepo, embedded in the binary. It reads the root f1.yml, shows what
// is deployed where, and drives deploys/rollbacks/env/logs by re-executing
// this same binary — so the web layer can never corrupt a deploy, and every
// action matches the CLI exactly.
package webui

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Alhasan-softwear/f1-runner/internal/config"
	"github.com/Alhasan-softwear/f1-runner/internal/deploy"
)

//go:embed index.html
var indexHTML []byte

type Options struct {
	ConfigPath string
	Listen     string
	Token      string
}

type server struct {
	opts      Options
	configAbs string
	configDir string

	jobsMu sync.Mutex
	jobs   []*job // newest last
	nextID int
	runMu  sync.Mutex // serializes deploy/rollback subprocesses
}

// job is one deploy/rollback run: buffered output plus live subscribers.
type job struct {
	ID      int       `json:"id"`
	Title   string    `json:"title"`
	Status  string    `json:"status"` // running | ok | failed
	Started time.Time `json:"started"`

	mu   sync.Mutex
	buf  strings.Builder
	subs map[chan string]bool
}

func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	j.buf.Write(p)
	for ch := range j.subs {
		select {
		case ch <- string(p):
		default: // slow subscriber: drop rather than stall the deploy
		}
	}
	j.mu.Unlock()
	return len(p), nil
}

func (j *job) snapshotAndSubscribe() (string, chan string) {
	ch := make(chan string, 256)
	j.mu.Lock()
	snap := j.buf.String()
	j.subs[ch] = true
	j.mu.Unlock()
	return snap, ch
}

func (j *job) unsubscribe(ch chan string) {
	j.mu.Lock()
	delete(j.subs, ch)
	j.mu.Unlock()
}

func (j *job) finish(status string) {
	j.mu.Lock()
	j.Status = status
	for ch := range j.subs {
		close(ch)
	}
	j.subs = map[chan string]bool{}
	j.mu.Unlock()
}

func Run(opts Options) error {
	abs, err := filepath.Abs(opts.ConfigPath)
	if err != nil {
		return err
	}
	if _, err := config.LoadRoot(abs); err != nil {
		return err // fail fast on a bad config; requests re-read it live
	}
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:9100"
	}
	if err := guardListen(opts.Listen, opts.Token); err != nil {
		return err
	}
	s := &server{opts: opts, configAbs: abs, configDir: filepath.Dir(abs)}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/api/overview", s.auth(s.handleOverview))
	mux.HandleFunc("/api/deploy", s.auth(s.handleDeploy))
	mux.HandleFunc("/api/rollback", s.auth(s.handleRollback))
	mux.HandleFunc("/api/env", s.auth(s.handleEnv))
	mux.HandleFunc("/api/logs", s.auth(s.handleLogs))
	mux.HandleFunc("/api/jobs", s.auth(s.handleJobs))
	mux.HandleFunc("/api/jobs/stream", s.auth(s.handleJobStream))

	httpServer := &http.Server{Addr: opts.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	url := opts.Listen
	if strings.HasPrefix(url, ":") {
		url = "localhost" + url
	}
	fmt.Printf("f1 dashboard: http://%s  (config %s)\n", url, abs)
	return httpServer.ListenAndServe()
}

// guardListen refuses to expose the dashboard beyond loopback without a token.
func guardListen(listen, token string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("bad --listen %q: %w", listen, err)
	}
	if token != "" {
		return nil
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("refusing to listen on %q without --token — the dashboard can deploy and read secrets", listen)
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		return fmt.Errorf("refusing to listen on non-loopback %q without --token", listen)
	}
	if ip := net.ParseIP(host); ip == nil && host != "localhost" {
		return fmt.Errorf("refusing to listen on %q without --token", listen)
	}
	return nil
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Token != "" {
			given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if given == "" {
				given = r.URL.Query().Get("token") // EventSource can't set headers
			}
			if subtle.ConstantTimeCompare([]byte(given), []byte(s.opts.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (s *server) loadConfig() (*config.Root, error) {
	return config.LoadRoot(s.configAbs)
}

// ---- overview ----

type overviewComponent struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Servers   []string `json:"servers"`
	DependsOn []string `json:"dependsOn,omitempty"`
	Runtime   string   `json:"runtime,omitempty"`
	Provision []string `json:"provision,omitempty"`
	BlueGreen bool     `json:"blueGreen,omitempty"`
	Shell     string   `json:"shell,omitempty"`
}

type overviewServer struct {
	Name string `json:"name"`
	Host string `json:"host"`
	User string `json:"user"`
	OS   string `json:"os"`
	Root string `json:"root"`
}

func (s *server) handleOverview(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var servers []overviewServer
	for _, name := range cfg.ServerNames() {
		sv := cfg.Servers[name]
		servers = append(servers, overviewServer{name, sv.Host, sv.User, sv.OS, sv.Root})
	}
	var comps []overviewComponent
	for _, name := range cfg.ComponentNames() {
		c := cfg.Components[name]
		oc := overviewComponent{Name: name, Path: c.Path, Servers: c.Servers, DependsOn: c.DependsOn}
		// Manifest details come from the local working tree; best-effort.
		if raw, err := os.ReadFile(filepath.Join(s.configDir, filepath.FromSlash(c.Path), "f1.yml")); err == nil {
			if m, err := config.ParseManifest(raw, name); err == nil {
				oc.Runtime = m.Runtime
				oc.Provision = m.Provision
				oc.BlueGreen = m.BlueGreen != nil
				oc.Shell = m.Shell
			}
		}
		comps = append(comps, oc)
	}
	rows, unreachable, err := deploy.FetchStatus(cfg, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"project":     cfg.Project,
		"repo":        cfg.Repo,
		"branch":      cfg.Branch,
		"servers":     servers,
		"components":  comps,
		"status":      rows,
		"unreachable": unreachable,
		"now":         time.Now().UTC().Format(time.RFC3339),
	})
}

// ---- jobs ----

func (s *server) newJob(title string) *job {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	s.nextID++
	j := &job{ID: s.nextID, Title: title, Status: "running", Started: time.Now(), subs: map[chan string]bool{}}
	s.jobs = append(s.jobs, j)
	if len(s.jobs) > 50 {
		s.jobs = s.jobs[len(s.jobs)-50:]
	}
	return j
}

func (s *server) findJob(id int) *job {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// runJob executes this binary with args from the monorepo dir, streaming into
// the job buffer. Deploy-class jobs are serialized.
func (s *server) runJob(j *job, args []string) {
	go func() {
		s.runMu.Lock()
		defer s.runMu.Unlock()
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintf(j, "ui error: %v\n", err)
			j.finish("failed")
			return
		}
		fmt.Fprintf(j, "$ f1 %s\n", strings.Join(args, " "))
		cmd := exec.Command(self, args...)
		cmd.Dir = s.configDir
		cmd.Stdout = j
		cmd.Stderr = j
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(j, "\n%v\n", err)
			j.finish("failed")
			return
		}
		j.finish("ok")
	}()
}

func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	s.jobsMu.Lock()
	list := make([]map[string]any, 0, len(s.jobs))
	for i := len(s.jobs) - 1; i >= 0; i-- { // newest first
		j := s.jobs[i]
		j.mu.Lock()
		list = append(list, map[string]any{
			"id": j.ID, "title": j.Title, "status": j.Status,
			"started": j.Started.UTC().Format(time.RFC3339),
		})
		j.mu.Unlock()
	}
	s.jobsMu.Unlock()
	writeJSON(w, list)
}

func (s *server) handleJobStream(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	j := s.findJob(id)
	if j == nil {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(event, data string) {
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		for _, line := range strings.Split(data, "\n") {
			fmt.Fprintf(w, "data: %s\n", line)
		}
		fmt.Fprint(w, "\n")
		fl.Flush()
	}

	snap, ch := j.snapshotAndSubscribe()
	defer j.unsubscribe(ch)
	if snap != "" {
		send("", snap)
	}
	j.mu.Lock()
	done := j.Status != "running"
	status := j.Status
	j.mu.Unlock()
	if done {
		send("done", status)
		return
	}
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case chunk, open := <-ch:
			if !open {
				j.mu.Lock()
				status := j.Status
				j.mu.Unlock()
				send("done", status)
				return
			}
			send("", chunk)
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ---- actions ----

func (s *server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Components []string `json:"components"`
		All        bool     `json:"all"`
		Ref        string   `json:"ref"`
		Force      bool     `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}
	if !req.All && len(req.Components) == 0 {
		http.Error(w, "pick components or all", http.StatusBadRequest)
		return
	}
	args := []string{"deploy", "--config", s.configAbs}
	title := "deploy "
	if req.All {
		args = append(args, "--all")
		title += "all"
	} else {
		args = append(args, req.Components...)
		title += strings.Join(req.Components, ", ")
	}
	if req.Ref != "" {
		args = append(args, "--ref", req.Ref)
		title += " @ " + req.Ref
	}
	if req.Force {
		args = append(args, "--force")
		title += " (force)"
	}
	j := s.newJob(title)
	s.runJob(j, args)
	writeJSON(w, map[string]any{"job": j.ID})
}

func (s *server) handleRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Component string `json:"component"`
		Server    string `json:"server"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Component == "" {
		http.Error(w, "component required", http.StatusBadRequest)
		return
	}
	args := []string{"rollback", req.Component, "--config", s.configAbs}
	title := "rollback " + req.Component
	if req.Server != "" {
		args = append(args, "--server", req.Server)
		title += " on " + req.Server
	}
	j := s.newJob(title)
	s.runJob(j, args)
	writeJSON(w, map[string]any{"job": j.ID})
}

// handleEnv: GET ?component=&server= shows; POST {component,op,args,server} edits.
func (s *server) handleEnv(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		comp := r.URL.Query().Get("component")
		if comp == "" {
			http.Error(w, "component required", http.StatusBadRequest)
			return
		}
		args := []string{"env", "show", comp, "--config", s.configAbs}
		if sv := r.URL.Query().Get("server"); sv != "" {
			args = append(args, "--server", sv)
		}
		out, err := s.runSync(args, time.Minute)
		writeJSON(w, map[string]any{"output": out, "ok": err == nil})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Component string   `json:"component"`
		Op        string   `json:"op"` // set | unset
		Args      []string `json:"args"`
		Server    string   `json:"server"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.Component == "" || (req.Op != "set" && req.Op != "unset") || len(req.Args) == 0 {
		http.Error(w, "need component, op set|unset, args", http.StatusBadRequest)
		return
	}
	args := []string{"env", req.Op, req.Component}
	args = append(args, req.Args...)
	args = append(args, "--config", s.configAbs)
	if req.Server != "" {
		args = append(args, "--server", req.Server)
	}
	out, err := s.runSync(args, time.Minute)
	writeJSON(w, map[string]any{"output": out, "ok": err == nil})
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	comp := r.URL.Query().Get("component")
	if comp == "" {
		http.Error(w, "component required", http.StatusBadRequest)
		return
	}
	n := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 5000 {
		n = v
	}
	args := []string{"logs", comp, "-n", strconv.Itoa(n), "--config", s.configAbs}
	if sv := r.URL.Query().Get("server"); sv != "" {
		args = append(args, "--server", sv)
	}
	out, err := s.runSync(args, 2*time.Minute)
	writeJSON(w, map[string]any{"output": out, "ok": err == nil})
}

// runSync executes a quick f1 subcommand and returns its combined output.
func (s *server) runSync(args []string, timeout time.Duration) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return err.Error(), err
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = s.configDir
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err.Error(), err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
		return buf.String() + "\n(timed out)", fmt.Errorf("timeout")
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
