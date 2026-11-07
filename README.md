# Caddy plugin: WeKan kanban

A [Caddy](https://caddyserver.com) plugin that **is** [WeKan](https://wekan.fi), the
kanban board, written in Go and running inside Caddy: no Node.js, no second process.
Caddy does TLS, HTTP/2 and HTTP/3; this plugin serves WeKan's pages and REST API, and
keeps WeKan's data in the same database Node.js WeKan uses - FerretDB over SQLite,
embedded by [caddy-ferretdb-plugin](https://github.com/wekan/caddy-ferretdb-plugin). Part
of [Weddy](https://github.com/wekan/weddy): Caddy with the WeKan, FerretDB,
[Gitea](https://github.com/wekan/caddy-gitea-plugin) and
[rclone](https://github.com/wekan/caddy-rclone-plugin) plugins.

| Module | What it does |
|--------|--------------|
| `wekan` (app) | opens WeKan's database in `<data>/files/db` when Caddy starts and closes it when Caddy stops |
| `http.handlers.wekan` | serves WeKan; `503` with `Retry-After` until the database is open |

## Caddyfile

```caddyfile
{
	wekan {
		data /var/lib/weddy
	}
}

wekan.example.org {
	wekan
}
```

| Option | Meaning |
|--------|---------|
| `data` | WeKan's files: `<data>/files/db/wekan.sqlite` is its database. Required, absolute. (`root` is its older name.) |
| `database_listen` | a TCP address, e.g. `127.0.0.1:27017`, where the database also answers - for `mongodump` and other MongoDB tools. One WeKan only |
| `primary` | turns on many domains, below |

The Node.js WeKan options of earlier versions - `bundle`, `command`, `args`, `dial`,
`env`, `root_url`, `stop_timeout` - are refused with that reason.

## The same data as Node.js WeKan

The `web` package reads and writes WeKan's own documents - `users`, `boards`,
`swimlanes`, `lists`, `cards` - in the same shapes, so a WeKan's `files/db` works here
as it is, and back:

- passwords are checked as Meteor's accounts-password stores them: bcrypt or argon2 of
  the password's SHA-256 digest;
- a login is a Meteor login token: `services.resume.loginTokens` keeps its hash, and the
  browser keeps it in the HttpOnly `meteor_login_token` cookie WeKan sets - valid 90
  days, as WeKan's default;
- new documents get Meteor's 17-character ids;
- a document is only ever changed with `$set` / `$push` of the fields this code knows,
  so a field it does not know yet is never lost.

## What it serves

| | |
|---|---|
| `/sign-in`, `/logout` | sign in with username or e-mail address; 5 failures in a row lock that address out for 15 minutes |
| `/` | All Boards: the boards the user is an active member of |
| `/b/<id>/<slug>`, `/b/<id>/<slug>/<card>` | the board - swimlanes, lists, cards - and a card's details over it. A public board is readable by everybody signed in, writable only by members |
| `POST /b/<id>/lists`, `/cards`, `/move` | add a list, add a card, move cards |
| `/b/<id>/live` | Server-Sent Events: `board-changed`, and every open page of the board redraws itself |
| `POST /users/login`, `/api/...` | WeKan's REST API at WeKan's addresses and in its shapes (below) |

The pages are [Go Pug](https://github.com/Joker/jade) templates (`web/templates/`)
compiled to `html/template` at start, made interactive with
[Unpoly](https://unpoly.com) and [interact.js](https://interactjs.io) - with WeKan's
**MultiDrag** from [wekan/wami](https://github.com/wekan/wami/tree/main/public/multidrag):
every finger, or the mouse, drags its own card at the same time, and Ctrl/Cmd-click
selects cards that move together. Both libraries are MIT and vendored in
`web/static/vendor/`. They keep WeKan's class names (`.js-list`, `.js-minicard`,
`.list-header-name`, `.js-card-details` ...), so WeKan's own Playwright page objects
find the same elements.

A form from another site is refused (its `Origin` must be this host), and the cookie is
`SameSite=Lax`.

### REST API

| | |
|---|---|
| `POST /users/login` | `{username or email, password}`, JSON or form → `{id, token, tokenExpires}`; the same lockout |
| `GET /api/user` | the signed-in user |
| `GET /api/users/<id>/boards` | `[{_id, title}]` - your own, or anybody's for an admin |
| `POST /api/boards` | `{title}` → `{_id, defaultSwimlaneId}` |
| `GET /api/boards/<id>` | the board |
| `GET /api/boards/<id>/swimlanes`, `/lists` | `[{_id, title}]` |
| `POST /api/boards/<id>/lists` | `{title}` → `{_id}` |
| `GET`/`POST /api/boards/<id>/lists/<list>/cards` | the cards; a new card `{title, description, swimlaneId}` - its author is always the caller |
| `GET`/`PUT`/`DELETE .../cards/<card>` | a card; `PUT` changes `title`, `description`, `listId`, `swimlaneId` |

`Authorization: Bearer <token>`. 401 without a valid token, 403 for a board the caller
may not read - or, for a change, is not an active member of.

## Translations

`web/i18n/` is WeKan's own translations - every language WeKan has, only the keys the
Go WeKan uses - copied by

```sh
go run ./cmd/sync-i18n /path/to/wekan
```

A translation is written in WeKan (`imports/i18n/data/`) and nowhere else. The language
is the user's `profile.language`, else the browser's, else English; right-to-left
languages get `dir="rtl"`. A test fails when a page uses a key that was not copied.

## Many domains: one Weddy, a WeKan per customer domain

```caddyfile
{
	on_demand_tls {
		permission wekan_domains
	}
	wekan {
		data    /var/lib/weddy
		primary wekan.team
	}
}
https:// {
	tls {
		on_demand
		get_certificate wekan_cloudflare
	}
	wekan
}
```

With `primary`, every domain is a WeKan of its own in this process, with its own
database in its own directory - `<data>/domains/customer.wekan.team/files/db/` - and a
request reaches only the WeKan of its own domain: an unknown or expired domain gets 404,
never another customer's WeKan, and a user of one domain does not exist at another.

| | |
|---|---|
| `primary` | the first domain: its site admins have **Admin Panel / People / Cloud** (`/admin/people/cloud`), where they name the Cloud Panel Domain and the SuperAdmin Username (a username or an e-mail address) |
| Cloud Panel Domain | served by the primary's WeKan; there the SuperAdmin has the **Cloud Panel** (`/cloud`): Settings with the left menus **Domains** (Domain, Customer name, Customer email address, Billing email address, Start Date, End Date, and TLS - what Weddy did, `!` when it failed) and **CloudFlare** (a write-only API key). Every request asks again: the SuperAdmin, at the Cloud Panel Domain; anybody else gets "The Cloud Panel is only for the SuperAdmin, at the Cloud Panel Domain" |
| a customer domain | a tenant: no Cloud menu, no SuperAdmin options |
| `<data>/cloud/settings.json` | the SuperAdmin Username (mode 0600) |
| `<data>/cloud/domains.json` | the Cloud Panel Domain and the Domains table, written whole by the Cloud Panel: a domain is served from its Start Date to its End Date, both included. Changes are followed within seconds |
| `<data>/cloud/cloudflare.json` | the CloudFlare API token (mode 0600), read here only - never sent to a browser |
| `<data>/cloud/status.json` | for each domain: CloudFlare or Let's Encrypt, its DNS record, its certificate, errors |

TLS needs no reload when domains change. `permission wekan_domains` lets Caddy get an
on-demand certificate only for a served domain. A domain whose zone is in the
CloudFlare account gets a proxied DNS record - a CNAME to the Cloud Panel Domain - and
a **CloudFlare Origin certificate** (key and CSR made here, renewed 30 days before it
ends), which `get_certificate wekan_cloudflare` serves; set the zone's SSL/TLS mode to
"Full (strict)". Every other served domain gets a **Let's Encrypt** certificate. The
token needs Zone:Read, DNS:Edit and SSL and Certificates:Edit.

## Status

The first part of WeKan is here: sign-in, All Boards, a board, a card, adding lists and
cards, moving cards, live updates, the REST API above, and the Cloud Panel. WeKan's 174 Jade templates
become Go Pug templates one page at a time; [TEMPLATES.md](TEMPLATES.md) lists each and
whether it is done.

## Building

With [Weddy](https://github.com/wekan/weddy), or with xcaddy, the repositories side by
side:

```sh
CGO_ENABLED=0 xcaddy build \
  --with github.com/wekan/caddy-wekan-plugin=. \
  --with github.com/wekan/caddy-ferretdb-plugin=../caddy-ferretdb-plugin \
  --replace github.com/FerretDB/FerretDB=../FerretDB
```

## Tests

```sh
CGO_ENABLED=0 go test ./...
```

`web/` runs the Go WeKan over a fresh embedded FerretDB: Meteor's password and token
formats, sign-in and its lockout, boards, lists, cards and MultiDrag moves, live
updates, the REST API, translations - each with negative tests: another user's board,
a move to another board's list, a form from another site, a made-up token. The plugin's
tests run a real Caddy: one WeKan, and many domains, each with its own database and
users.

## License

MIT - see [LICENSE](LICENSE). Unpoly and interact.js: MIT, their licenses beside them in
`web/static/vendor/`.

Maintainer and contributor rules are in
[WeKan's CLAUDE.md and AGENTS.md](https://github.com/wekan/wekan), which cover every
repository WeKan clones into its `.tools/` directory.
