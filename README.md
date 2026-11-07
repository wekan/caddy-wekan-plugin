# Caddy plugin: WeKan kanban

A [Caddy](https://caddyserver.com) plugin that runs [WeKan](https://wekan.fi), the
kanban board, inside Caddy's lifetime and serves it through Caddy - one command starts
both, Caddy does TLS, HTTP/2 and HTTP/3 in front of WeKan, and stopping Caddy stops
WeKan and its database. Part of [Weddy](https://github.com/wekan/weddy), Caddy with the
WeKan and [Gitea](https://github.com/wekan/caddy-gitea-plugin) plugins.

| Module | What it does |
|--------|--------------|
| `wekan` (app) | starts a WeKan release bundle with the bundle's own `start-wekan.sh` (`start-wekan.bat` on Windows) as a supervised child process, restarts it when it exits on its own, and stops it - and the FerretDB it started - when Caddy stops |
| `http.handlers.wekan` | serves a site from that WeKan with Caddy's reverse proxy, WebSockets included; answers `503` with `Retry-After` while WeKan is starting |

The bundle's own launcher does what it always does - starts the bundled FerretDB with
SQLite, its startup recovery and restore requests, or uses `MONGO_URL` - so nothing of
it is repeated here. WeKan stays its own program, a separate, versioned process (WeKan's
[docs/Design/Multiverse/Go.md](https://github.com/wekan/wekan/blob/main/docs/Design/Multiverse/Go.md)).

## Caddyfile

```caddyfile
{
	wekan {
		bundle   /opt/wekan
		root_url https://wekan.example.org
	}
}

wekan.example.org {
	wekan
}
```

| Option | Meaning | Default |
|--------|---------|---------|
| `bundle` | the WeKan release bundle directory | required |
| `root_url` | the address users open WeKan at - links, e-mails and cookies use it | required |
| `dial` | where WeKan listens: `host:port` (given to WeKan as `BIND_IP` and `PORT`) or `unix//path` (as `UNIX_SOCKET_PATH`) | `127.0.0.1:8080` |
| `command`, `args` | another launcher | `<bundle>/start-wekan.sh` |
| `env <NAME> <value>` | WeKan's settings: `WEKAN_DB`, `MONGO_URL`, `WRITABLE_PATH`, ... | - |
| `stop_timeout` | how long WeKan and FerretDB get to stop | `60s` |

`env` cannot change where WeKan listens; `dial` decides that, so the proxy finds it.
WeKan listens on 127.0.0.1 by default - Meteor's own default would be every address.

## Uses caddy-gitea-plugin

The process supervisor and the proxy are caddy-gitea-plugin's
[`supervisor`](https://github.com/wekan/caddy-gitea-plugin/tree/main/supervisor) and
[`proxy`](https://github.com/wekan/caddy-gitea-plugin/tree/main/proxy) packages - one
Caddy plugin using parts of another, as Weddy's design says. Importing them does not
add Gitea to Caddy. Both repositories are checked out side by side (`go.mod` replaces
it with `../caddy-gitea-plugin`), as in WeKan's `.tools/w/`, Weddy's build and this
repository's tests workflow.

## Building

With [Weddy](https://github.com/wekan/weddy), or with xcaddy:

```sh
xcaddy build --with github.com/wekan/caddy-wekan-plugin=. --with github.com/wekan/caddy-gitea-plugin=../caddy-gitea-plugin
```

## Tests

```sh
go test ./...
```

They run a real Caddy with a stand-in bundle whose `start-wekan.sh` starts a
background process, as the real one starts FerretDB, and then WeKan: `ROOT_URL` and the
path reach WeKan, and after Caddy stops, both are gone.

## License

MIT - see [LICENSE](LICENSE).

Maintainer and contributor rules are in
[WeKan's CLAUDE.md and AGENTS.md](https://github.com/wekan/wekan), which cover every
repository WeKan clones into its `.tools/` directory.
