package caddywekan

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/wekan/caddy-gitea-plugin/proxy"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("wekan", parseGlobal)
	httpcaddyfile.RegisterHandlerDirective("wekan", parseHandler)
	httpcaddyfile.RegisterDirectiveOrder("wekan", httpcaddyfile.Before, "reverse_proxy")
}

// Handler serves a site from the WeKan that the wekan app runs - with root, from
// the WeKan of the request's domain.
type Handler struct {
	proxy *proxy.Proxy
	cloud *Cloud
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.wekan", New: func() caddy.Module { return new(Handler) }}
}

// Provision connects the handler to the wekan app.
func (h *Handler) Provision(ctx caddy.Context) error {
	v, err := ctx.AppIfConfigured("wekan")
	if err != nil {
		return err // "app module wekan: not configured": add the wekan global option
	}
	app := v.(*App)
	if app.cloud != nil {
		h.cloud = app.cloud
		return nil
	}
	h.proxy, err = proxy.New(ctx, "WeKan", app.Dial, app.Ready)
	return err
}

// ServeHTTP proxies the request to WeKan.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.cloud != nil {
		if handled, status := h.cloud.ServeHTTP(w, r); !handled {
			if status == http.StatusNotFound {
				return caddyhttp.Error(status, fmt.Errorf("no WeKan is served at %s", NormalizeHost(r.Host)))
			}
			return caddyhttp.Error(status, fmt.Errorf("the WeKan of %s is starting", NormalizeHost(r.Host)))
		}
		return nil
	}
	return h.proxy.ServeHTTP(w, r, next)
}

// Cleanup releases the reverse proxy.
func (h *Handler) Cleanup() error {
	if h.proxy == nil {
		return nil
	}
	return h.proxy.Cleanup()
}

// UnmarshalCaddyfile: the directive takes no arguments, "wekan".
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() || d.NextBlock(0) {
		return d.Err("wekan takes no arguments: its settings are in the global wekan option")
	}
	return nil
}

func parseHandler(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler Handler
	return &handler, handler.UnmarshalCaddyfile(h.Dispenser)
}

// UnmarshalCaddyfile reads the global option:
//
//	wekan {
//		bundle       <dir>
//		root_url     <https://...>
//		command      <path>
//		args         <arg>...
//		dial         <host:port | unix//path>
//		env          <KEY> <value>
//		stop_timeout <duration>
//	}
func (a *App) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		key := d.Val()
		switch key {
		case "bundle", "root_url", "command", "dial", "root", "primary":
			if !d.NextArg() {
				return d.ArgErr()
			}
			v := d.Val()
			switch key {
			case "bundle":
				a.Bundle = v
			case "root_url":
				a.RootURL = v
			case "command":
				a.Command = v
			case "dial":
				a.Dial = v
			case "root":
				a.Root = v
			case "primary":
				a.Primary = v
			}
			if d.NextArg() {
				return d.ArgErr()
			}
		case "args":
			a.Args = append(a.Args, d.RemainingArgs()...)
		case "env":
			args := d.RemainingArgs()
			if len(args) < 2 {
				return d.Err("env needs a name and a value")
			}
			if a.Env == nil {
				a.Env = map[string]string{}
			}
			a.Env[args[0]] = strings.Join(args[1:], " ")
		case "stop_timeout":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("stop_timeout: %v", err)
			}
			a.StopTimeout = caddy.Duration(dur)
		default:
			return d.Errf("unknown wekan option %q", key)
		}
	}
	return nil
}

func parseGlobal(d *caddyfile.Dispenser, _ any) (any, error) {
	var app App
	if err := app.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}
	return httpcaddyfile.App{Name: "wekan", Value: caddyconfig.JSON(app, nil)}, nil
}

var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*App)(nil)
)
