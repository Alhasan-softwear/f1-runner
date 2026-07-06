// f1 — push a monorepo, run every component where it belongs.
//
// One binary, three roles: on your machine it plans and fans out over SSH; on
// each server it fetches the repo and runs component lifecycles; and it can
// stay resident as a webhook agent for CI-triggered deploys.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Alhasan-softwear/f1-runner/internal/agent"
	"github.com/Alhasan-softwear/f1-runner/internal/config"
	"github.com/Alhasan-softwear/f1-runner/internal/deploy"
	"github.com/Alhasan-softwear/f1-runner/internal/provision"
	"github.com/Alhasan-softwear/f1-runner/internal/ui"
	"github.com/Alhasan-softwear/f1-runner/internal/webui"
)

var version = "0.3.0"

const usage = `f1 %s — deploy a monorepo to one or many servers

Usage on your machine (needs f1.yml in the current directory):
  f1 init                      scaffold a root f1.yml
  f1 init component <path>     scaffold a component f1.yml at <path>
  f1 server setup [names…]     prepare server(s): layout, binary, deploy key, provisioning
  f1 deploy <comps…|--all>     deploy components  [--ref R] [--force] [--dry-run]
  f1 status [--server S]       what is deployed where
  f1 logs <comp> [-n N] [-f]   tail a component's logs  [--server S]
  f1 rollback <comp>           instant rollback to the previous release  [--server S]
  f1 env set <comp> K=V …      set server-side secrets   [--server S]
  f1 env unset <comp> K …      remove keys               [--server S]
  f1 env show <comp>           print the env file        [--server S]
  f1 ui                        web dashboard             [--listen 127.0.0.1:9100] [--token T]
  f1 version

On servers (run automatically over SSH; also usable manually or from cron):
  f1 apply --root /opt/f1 --repo URL --ref R [--components a,b] [--force] [--dry-run]
  f1 agent --root /opt/f1 --repo URL --token SECRET [--listen :9123] [--branch main]
  f1 provision --local pkg[,pkg…]      install runtimes (%s)
  f1 status|logs|rollback|env --local --root /opt/f1
`

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "deploy":
		err = cmdDeploy(os.Args[2:])
	case "apply":
		err = cmdApply(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "logs":
		err = cmdLogs(os.Args[2:])
	case "rollback":
		err = cmdRollback(os.Args[2:])
	case "server":
		err = cmdServer(os.Args[2:])
	case "env":
		err = cmdEnv(os.Args[2:])
	case "agent":
		err = cmdAgent(os.Args[2:])
	case "ui":
		err = cmdUI(os.Args[2:])
	case "provision":
		err = cmdProvision(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		printUsage()
	default:
		printUsage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		ui.Errorf("%v", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(usage, version, strings.Join(provision.Known(), ", "))
}

func loadRoot(path string) (*config.Root, error) {
	cfg, err := config.LoadRoot(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no %s here — run this from your monorepo root (or scaffold one with `f1 init`)", path)
	}
	return cfg, err
}

// splitLeadingArgs peels positional words off the front so both
// `f1 logs web -f` and `f1 logs -f web` work with stdlib flag.
func splitLeadingArgs(args []string) ([]string, []string) {
	var positional []string
	rest := args
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	return positional, rest
}

func cmdDeploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	all := fs.Bool("all", false, "deploy every component")
	ref := fs.String("ref", "", "branch, tag, or commit sha (default: branch from f1.yml)")
	force := fs.Bool("force", false, "deploy even if nothing changed")
	dryRun := fs.Bool("dry-run", false, "show what would happen without changing anything")
	cfgPath := fs.String("config", "f1.yml", "path to the root config")
	fs.Parse(args)
	comps := fs.Args()
	if !*all && len(comps) == 0 {
		return fmt.Errorf("name components to deploy, or use --all")
	}
	cfg, err := loadRoot(*cfgPath)
	if err != nil {
		return err
	}
	return deploy.Deploy(cfg, deploy.DeployOptions{
		Components: comps, All: *all, Ref: *ref, Force: *force, DryRun: *dryRun,
	})
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	root := fs.String("root", "/opt/f1", "f1 root directory on this machine")
	repo := fs.String("repo", "", "git URL or local path of the monorepo")
	ref := fs.String("ref", "main", "branch, tag, or commit sha")
	compsCSV := fs.String("components", "", "comma-separated component names (default: all)")
	force := fs.Bool("force", false, "deploy even if nothing changed")
	dryRun := fs.Bool("dry-run", false, "show what would happen")
	fs.Parse(args)
	if *repo == "" {
		return fmt.Errorf("--repo is required")
	}
	var comps []string
	if *compsCSV != "" {
		comps = strings.Split(*compsCSV, ",")
	}
	return deploy.Apply(deploy.ApplyOptions{
		Root: *root, RepoURL: *repo, Ref: *ref, Components: comps, Force: *force, DryRun: *dryRun,
	})
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	local := fs.Bool("local", false, "read this machine's state instead of querying servers")
	asJSON := fs.Bool("json", false, "JSON output (with --local)")
	root := fs.String("root", "/opt/f1", "f1 root (with --local)")
	server := fs.String("server", "", "only this server")
	cfgPath := fs.String("config", "f1.yml", "path to the root config")
	fs.Parse(args)
	if *local {
		return deploy.StatusLocal(*root, *asJSON)
	}
	cfg, err := loadRoot(*cfgPath)
	if err != nil {
		return err
	}
	return deploy.Status(cfg, *server)
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	local := fs.Bool("local", false, "read logs on this machine")
	root := fs.String("root", "/opt/f1", "f1 root (with --local)")
	server := fs.String("server", "", "only this server")
	lines := fs.Int("n", 100, "number of lines")
	follow := fs.Bool("f", false, "follow")
	cfgPath := fs.String("config", "f1.yml", "path to the root config")
	comps, rest := splitLeadingArgs(args)
	fs.Parse(rest)
	comps = append(comps, fs.Args()...)
	if len(comps) != 1 {
		return fmt.Errorf("usage: f1 logs <component> [-n N] [-f] [--server S]")
	}
	if *local {
		return deploy.LogsLocal(*root, comps[0], *lines, *follow)
	}
	cfg, err := loadRoot(*cfgPath)
	if err != nil {
		return err
	}
	return deploy.Logs(cfg, comps[0], *server, *lines, *follow)
}

func cmdRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	local := fs.Bool("local", false, "roll back on this machine")
	root := fs.String("root", "/opt/f1", "f1 root (with --local)")
	server := fs.String("server", "", "only this server")
	cfgPath := fs.String("config", "f1.yml", "path to the root config")
	comps, rest := splitLeadingArgs(args)
	fs.Parse(rest)
	comps = append(comps, fs.Args()...)
	if len(comps) != 1 {
		return fmt.Errorf("usage: f1 rollback <component> [--server S]")
	}
	if *local {
		return deploy.RollbackLocal(*root, comps[0])
	}
	cfg, err := loadRoot(*cfgPath)
	if err != nil {
		return err
	}
	return deploy.Rollback(cfg, comps[0], *server)
}

func cmdServer(args []string) error {
	if len(args) < 1 || args[0] != "setup" {
		return fmt.Errorf("usage: f1 server setup [names…] [--binary path]")
	}
	fs := flag.NewFlagSet("server setup", flag.ExitOnError)
	binary := fs.String("binary", "", "local path to a server f1 binary to upload")
	cfgPath := fs.String("config", "f1.yml", "path to the root config")
	names, rest := splitLeadingArgs(args[1:])
	fs.Parse(rest)
	names = append(names, fs.Args()...)
	cfg, err := loadRoot(*cfgPath)
	if err != nil {
		return err
	}
	return deploy.ServerSetup(cfg, deploy.SetupOptions{Servers: names, Binary: *binary})
}

func cmdEnv(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: f1 env set|unset|show <component> [KEY=VALUE|KEY …] [--server S]")
	}
	op := args[0]
	if op != "set" && op != "unset" && op != "show" {
		return fmt.Errorf("unknown env operation %q (set, unset, or show)", op)
	}
	fs := flag.NewFlagSet("env "+op, flag.ExitOnError)
	local := fs.Bool("local", false, "operate on this machine's env files")
	root := fs.String("root", "/opt/f1", "f1 root (with --local)")
	server := fs.String("server", "", "only this server")
	cfgPath := fs.String("config", "f1.yml", "path to the root config")
	positional, rest := splitLeadingArgs(args[1:])
	fs.Parse(rest)
	positional = append(positional, fs.Args()...)
	if len(positional) < 1 {
		return fmt.Errorf("usage: f1 env %s <component> …", op)
	}
	comp, kvArgs := positional[0], positional[1:]
	switch {
	case op == "set" && len(kvArgs) == 0:
		return fmt.Errorf("usage: f1 env set <component> KEY=VALUE [KEY=VALUE…]")
	case op == "unset" && len(kvArgs) == 0:
		return fmt.Errorf("usage: f1 env unset <component> KEY [KEY…]")
	case op == "show" && len(kvArgs) > 0:
		return fmt.Errorf("usage: f1 env show <component>")
	}
	if *local {
		switch op {
		case "set":
			return deploy.EnvSetLocal(*root, comp, kvArgs)
		case "unset":
			return deploy.EnvUnsetLocal(*root, comp, kvArgs)
		default:
			return deploy.EnvShowLocal(*root, comp)
		}
	}
	cfg, err := loadRoot(*cfgPath)
	if err != nil {
		return err
	}
	return deploy.Env(cfg, op, comp, *server, kvArgs)
}

func cmdAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	root := fs.String("root", "/opt/f1", "f1 root directory on this machine")
	repo := fs.String("repo", "", "git URL or local path of the monorepo")
	listen := fs.String("listen", ":9123", "listen address")
	token := fs.String("token", os.Getenv("F1_AGENT_TOKEN"), "auth token (or F1_AGENT_TOKEN)")
	branch := fs.String("branch", "main", "branch whose pushes trigger deploys")
	compsCSV := fs.String("components", "", "restrict which components this agent deploys")
	fs.Parse(args)
	var comps []string
	if *compsCSV != "" {
		comps = strings.Split(*compsCSV, ",")
	}
	return agent.Run(agent.Options{
		Root: *root, RepoURL: *repo, Branch: *branch, Listen: *listen, Token: *token, Components: comps,
	})
}

func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:9100", "listen address (non-loopback requires --token)")
	tok := fs.String("token", os.Getenv("F1_UI_TOKEN"), "auth token (or F1_UI_TOKEN); required off-loopback")
	cfgPath := fs.String("config", "f1.yml", "path to the root config")
	fs.Parse(args)
	if _, err := loadRoot(*cfgPath); err != nil {
		return err
	}
	return webui.Run(webui.Options{ConfigPath: *cfgPath, Listen: *listen, Token: *tok})
}

func cmdProvision(args []string) error {
	fs := flag.NewFlagSet("provision", flag.ExitOnError)
	local := fs.Bool("local", false, "install on this machine (servers get this via `f1 server setup`)")
	fs.Parse(args)
	pkgs := strings.Join(fs.Args(), ",")
	if !*local {
		return fmt.Errorf("f1 provision runs on servers: use --local there, or list packages under provision: in f1.yml and run `f1 server setup`")
	}
	if pkgs == "" {
		return fmt.Errorf("usage: f1 provision --local pkg[,pkg…]  (known: %s)", strings.Join(provision.Known(), ", "))
	}
	return provision.Ensure(strings.Split(pkgs, ","), os.Stdout)
}

const rootTemplate = `# f1 root config — commit this at your monorepo root.
project: myapp
repo: git@github.com:you/myapp.git   # servers fetch from here (deploy key) — or a server-local path
branch: main

servers:
  web1:
    host: 1.2.3.4
    user: deploy
    provision: [git, docker]        # installed by ` + "`f1 server setup`" + `
    # port: 22
    # key: ~/.ssh/id_ed25519        # identity file for YOUR ssh to the server
    # root: /opt/f1                 # where f1 lives on the server
    # os: windows                   # experimental windows servers
    # ssh_opts: ["-o", "StrictHostKeyChecking=accept-new"]

components:
  web:
    path: apps/web                  # subdirectory with its own f1.yml
    servers: [web1]                 # one component can target many servers
    # depends_on: [api]             # deployed in dependency order (waves)
`

const componentTemplate = `# f1 component manifest — lives inside the component directory.
name: %s
runtime: docker                     # docker | script

docker:
  compose: docker-compose.yml       # relative to this directory

# provision: [python, node@22, php, apache, mariadb, postgres, redis, nginx]
#                                   # runtimes f1 installs on the server first

# blue_green:                       # zero-downtime slot deploys (docker only)
#   ports: [8001, 8002]             # compose must publish "${F1_PORT}:80"
#   switch: ./scripts/switch.sh     # move traffic (e.g. rewrite nginx upstream)

# runtime: script components use lifecycle commands instead (run from the
# release directory, with $F1_RELEASE $F1_SHARED $F1_LOG $F1_REF set):
# shell: sh                         # sh | bash | cmd | powershell
# scripts:
#   setup: ./scripts/setup.sh
#   build: ./scripts/build.sh
#   start: ./scripts/start.sh       # must return — daemonize with nohup/systemd
#   stop:  ./scripts/stop.sh
#   logs:  tail -n 100 $F1_LOG

# env_file: /opt/f1/env/%s.env      # explicit secrets file; default is the
#                                   # one managed by ` + "`f1 env set %s KEY=VALUE`" + `

health:
  url: http://localhost:8080/       # or cmd: "curl -fsS http://localhost:8080/"
  retries: 5
  interval: 3s

# keep: 5                           # releases kept for rollback
`

func cmdInit(args []string) error {
	if len(args) >= 1 && args[0] == "component" {
		if len(args) < 2 {
			return fmt.Errorf("usage: f1 init component <path>")
		}
		dir := args[1]
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		dest := filepath.Join(dir, "f1.yml")
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("%s already exists", dest)
		}
		name := strings.ToLower(filepath.Base(filepath.Clean(dir)))
		content := fmt.Sprintf(componentTemplate, name, name, name)
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			return err
		}
		ui.Printf("wrote %s — edit it, then add the component to your root f1.yml", dest)
		return nil
	}
	if _, err := os.Stat("f1.yml"); err == nil {
		return fmt.Errorf("f1.yml already exists here")
	}
	if err := os.WriteFile("f1.yml", []byte(rootTemplate), 0o644); err != nil {
		return err
	}
	ui.Printf("wrote f1.yml — edit servers and components, then run `f1 server setup`")
	return nil
}
