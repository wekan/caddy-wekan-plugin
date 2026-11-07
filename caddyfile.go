package caddywekan

import (
	"fmt"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("wekan", parseGlobal)
	httpcaddyfile.RegisterHandlerDirective("wekan", parseHandler)
	httpcaddyfile.RegisterDirectiveOrder("wekan", httpcaddyfile.Before, "reverse_proxy")
}

// Handler serves a site from WeKan - with primary, from the WeKan of the
// request's domain.
type Handler struct {
	app *App
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
	h.app = v.(*App)
	return nil
}

// ServeHTTP answers with WeKan.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.app.cloud != nil {
		if handled, status := h.app.cloud.ServeHTTP(w, r); !handled {
			return caddyhttp.Error(status, fmt.Errorf("no WeKan is served at %s", NormalizeHost(r.Host)))
		}
		return nil
	}
	s := h.app.server
	if s == nil {
		w.Header().Set("Retry-After", "5")
		return caddyhttp.Error(http.StatusServiceUnavailable, fmt.Errorf("WeKan is starting"))
	}
	s.ServeHTTP(w, r)
	return nil
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
//		data            <dir>
//		primary         <first domain>
//		database_listen <host:port>
//	}
//
// root is the older name of data. The options of the Node.js WeKan that Weddy
// used to start - bundle, command, args, dial, env, root_url, stop_timeout - are
// refused with the reason: WeKan runs in Go inside Weddy now.
func (a *App) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		key := d.Val()
		switch key {
		case "data", "root", "primary", "database_listen":
			if !d.NextArg() {
				return d.ArgErr()
			}
			v := d.Val()
			switch key {
			case "data", "root":
				a.Data = v
			case "primary":
				a.Primary = v
			case "database_listen":
				a.DatabaseListen = v
			}
			if d.NextArg() {
				return d.ArgErr()
			}
		case "bundle", "command", "args", "dial", "env", "root_url", "stop_timeout":
			return d.Errf("wekan option %q is gone: Weddy runs WeKan in Go, without Node.js - set data, the directory of WeKan's files", key)
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
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*App)(nil)
)
