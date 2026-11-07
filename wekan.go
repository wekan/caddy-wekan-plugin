// Package caddywekan is a Caddy plugin that is WeKan, the kanban board, written
// in Go and running inside Caddy - no Node.js, no second process:
//
//   - the app "wekan" opens WeKan's database - FerretDB over SQLite, embedded by
//     caddy-ferretdb-plugin - in <data>/files/db, the same wekan.sqlite Node.js
//     WeKan writes, and stops it when Caddy stops;
//   - the handler "wekan" (http.handlers.wekan) serves WeKan's pages and REST API
//     (the web package).
//
// Caddyfile:
//
//	{
//		wekan {
//			data /var/lib/weddy
//		}
//	}
//	wekan.example.org {
//		wekan
//	}
//
// With primary, every domain is a WeKan of its own (cloud.go).
package caddywekan

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/exp/zapslog"

	"github.com/wekan/caddy-wekan-plugin/web"
)

func init() {
	caddy.RegisterModule(App{})
	caddy.RegisterModule(Handler{})
}

// App runs WeKan.
type App struct {
	// Data is the directory of WeKan's files: <data>/files/db is its database. With
	// primary, <data>/domains/<domain>/ is each domain's, and <data>/cloud/ the
	// Cloud Panel's Domains table and CloudFlare token.
	Data string `json:"data,omitempty"`
	// Primary is the first domain, and turns on many domains: its WeKan has Admin
	// Panel / People / Cloud, and the Cloud Panel on the Cloud Panel Domain.
	Primary string `json:"primary,omitempty"`
	// DatabaseListen is a TCP address where the database also answers, e.g.
	// 127.0.0.1:27017, for mongodump and other MongoDB tools. Only without primary:
	// each domain's database is its own.
	DatabaseListen string `json:"database_listen,omitempty"`

	server *web.Server
	cloud  *Cloud
	log    *zap.Logger
	slog   *slog.Logger
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "wekan", New: func() caddy.Module { return new(App) }}
}

// Provision prepares the settings; nothing is opened until Start.
func (a *App) Provision(ctx caddy.Context) error {
	a.log = ctx.Logger()
	a.slog = slog.New(zapslog.NewHandler(a.log.Core()))
	if a.Primary != "" {
		a.Primary = NormalizeHost(a.Primary)
		a.cloud = newCloud(a, a.log)
	}
	return nil
}

// Validate checks the settings before anything starts.
func (a *App) Validate() error {
	if a.Data == "" || !filepath.IsAbs(a.Data) {
		return fmt.Errorf("wekan: data must be the absolute path of WeKan's files, e.g. /var/lib/weddy (got %q)", a.Data)
	}
	if a.Primary != "" && !ValidDomain(a.Primary) {
		return fmt.Errorf("wekan: primary must be the first domain, e.g. wekan.example.org (got %q)", a.Primary)
	}
	if a.DatabaseListen != "" {
		if a.Primary != "" {
			return fmt.Errorf("wekan: database_listen is for one WeKan; with primary every domain's database is its own")
		}
		if _, _, err := net.SplitHostPort(a.DatabaseListen); err != nil {
			return fmt.Errorf("wekan: database_listen %q: %w", a.DatabaseListen, err)
		}
	}
	return nil
}

// Start opens WeKan's database and starts serving - or, with primary, every domain's.
func (a *App) Start() error {
	if a.cloud != nil {
		return a.cloud.Start()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := web.Open(ctx, a.Data, a.DatabaseListen, a.slog)
	if err != nil {
		return fmt.Errorf("wekan: %w", err)
	}
	a.server = s
	return nil
}

// Stop stops serving and closes the database - every domain's - when its last
// request is done.
func (a *App) Stop() error {
	if a.cloud != nil {
		return a.cloud.Stop()
	}
	if a.server == nil {
		return nil
	}
	err := a.server.Close()
	a.server = nil
	return err
}

var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
	_ caddy.Validator   = (*App)(nil)
)
