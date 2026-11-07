// Package caddywekan is a Caddy plugin that runs WeKan, the kanban board, inside
// Caddy's lifetime and serves it through Caddy:
//
//   - the app "wekan" starts a WeKan release bundle with the bundle's own
//     start-wekan.sh (start-wekan.bat on Windows) as a supervised child process -
//     which starts its bundled FerretDB and SQLite recovery the way WeKan always does -
//     restarts it when it exits, and stops it, FerretDB included, when Caddy stops;
//   - the handler "wekan" (http.handlers.wekan) proxies requests to it with Caddy's
//     reverse proxy, WebSockets (DDP) included.
//
// Caddyfile:
//
//	{
//		wekan {
//			bundle   /opt/wekan
//			root_url https://wekan.example.org
//		}
//	}
//	wekan.example.org {
//		wekan
//	}
//
// It uses the supervisor and proxy packages of caddy-gitea-plugin: one Caddy plugin
// using parts of another (wekan/weddy's design). WeKan stays its own program
// (WeKan's docs/Design/Multiverse/Go.md: a separate, versioned process).
package caddywekan

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"

	"github.com/wekan/caddy-gitea-plugin/supervisor"
)

func init() {
	caddy.RegisterModule(App{})
	caddy.RegisterModule(Handler{})
}

// App runs WeKan.
type App struct {
	// Bundle is the directory of a WeKan release bundle.
	Bundle string `json:"bundle,omitempty"`
	// Command starts WeKan. Default: <bundle>/start-wekan.sh, on Windows
	// <bundle>/start-wekan.bat.
	Command string `json:"command,omitempty"`
	// Args are given to Command.
	Args []string `json:"args,omitempty"`
	// RootURL is the address users open WeKan at, e.g. https://wekan.example.org:
	// WeKan's links, e-mails and cookies use it. Required.
	RootURL string `json:"root_url,omitempty"`
	// Dial is where WeKan listens: "host:port" (default 127.0.0.1:8080, given to
	// WeKan as BIND_IP and PORT) or "unix//path" (as UNIX_SOCKET_PATH).
	Dial string `json:"dial,omitempty"`
	// Env is added to WeKan's environment: MONGO_URL, WRITABLE_PATH, WEKAN_DB and
	// the rest of WeKan's settings.
	Env map[string]string `json:"env,omitempty"`
	// StopTimeout is how long WeKan and FerretDB get to stop. Default 60s: FerretDB
	// finishes its SQLite writes.
	StopTimeout caddy.Duration `json:"stop_timeout,omitempty"`

	proc *supervisor.Process
	log  *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "wekan", New: func() caddy.Module { return new(App) }}
}

// Provision works out the command and its environment.
func (a *App) Provision(ctx caddy.Context) error {
	a.log = ctx.Logger()
	if a.Dial == "" {
		a.Dial = "127.0.0.1:8080"
	}
	if a.Command == "" && a.Bundle != "" {
		launcher := "start-wekan.sh"
		if runtime.GOOS == "windows" {
			launcher = "start-wekan.bat"
		}
		a.Command = filepath.Join(a.Bundle, launcher)
	}
	if a.StopTimeout == 0 {
		a.StopTimeout = caddy.Duration(60 * time.Second)
	}
	env, err := a.Environment()
	if err != nil {
		return err
	}
	a.proc = supervisor.New(supervisor.Config{
		Name:        "wekan",
		Command:     a.Command,
		Args:        a.Args,
		Dir:         a.Bundle,
		Env:         env,
		Dial:        a.Dial,
		StopTimeout: time.Duration(a.StopTimeout),
	}, a.log)
	return nil
}

// Environment is what WeKan gets on top of Caddy's environment: ROOT_URL, where to
// listen, and Env. Env cannot override where WeKan listens - the proxy would not
// find it.
func (a *App) Environment() ([]string, error) {
	vals := map[string]string{}
	for k, v := range a.Env {
		vals[k] = v
	}
	vals["ROOT_URL"] = a.RootURL
	if rest, ok := cutUnix(a.Dial); ok {
		vals["UNIX_SOCKET_PATH"] = rest
		delete(vals, "PORT")
		delete(vals, "BIND_IP")
	} else {
		host, port, err := net.SplitHostPort(a.Dial)
		if err != nil {
			return nil, fmt.Errorf("wekan: dial %q: %w", a.Dial, err)
		}
		vals["BIND_IP"], vals["PORT"] = host, port
		delete(vals, "UNIX_SOCKET_PATH")
	}
	out := make([]string, 0, len(vals))
	for k, v := range vals {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out, nil
}

func cutUnix(dial string) (string, bool) {
	network, address := supervisor.DialNetwork(dial)
	return address, network == "unix"
}

// Validate checks the settings before anything starts.
func (a *App) Validate() error {
	if a.Command == "" {
		return fmt.Errorf("wekan: set bundle, the WeKan release bundle directory (or command)")
	}
	u, err := url.Parse(a.RootURL)
	if a.RootURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("wekan: root_url must be the address users open WeKan at, e.g. https://wekan.example.org (got %q)", a.RootURL)
	}
	if a.Bundle != "" {
		if st, err := os.Stat(a.Bundle); err != nil || !st.IsDir() {
			return fmt.Errorf("wekan: bundle %s is not a directory", a.Bundle)
		}
	}
	return nil
}

// Start starts WeKan.
func (a *App) Start() error { return a.proc.Start() }

// Stop stops WeKan and its FerretDB, and waits until they have exited.
func (a *App) Stop() error { return a.proc.Stop() }

// Ready reports whether WeKan accepts connections.
func (a *App) Ready() bool { return a.proc != nil && a.proc.Ready() }

var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
	_ caddy.Validator   = (*App)(nil)
)
