# wakil-mcp

One MCP endpoint in front of every MCP server you use.

Add each upstream server to wakil-mcp once, then point every harness —
Claude Desktop, Cursor, VS Code, opencode, your own agent — at a single URL.
The tools of every enabled server appear there, namespaced by the name you
gave them:

```
github.create_issue        <- the "github" server's create_issue
github.search_repos        <- the "github" server's search_repos
postgres.query             <- the "postgres" server's query
```

Adding a server takes effect immediately: no restart, no per-harness edit.
Remove it again and its tools disappear from every harness at once.

```
              ┌──────────────┐
 Claude    ──▶│              │──▶ github      (stdio, npx)
 Cursor    ──▶│   wakil-mcp  │──▶ postgres    (stdio, docker)
 opencode  ──▶│   :8765/mcp  │──▶ notion      (Streamable HTTP)
 your code ──▶│              │──▶ anything else that speaks MCP
              └──────────────┘
```

## Contents

- [Why](#why)
- [Quick start](#quick-start)
- [Everyday use](#everyday-use)
- [The web UI](#the-web-ui)
- [Tool names](#tool-names)
- [Connecting a harness](#connecting-a-harness)
- [Configuration reference](#configuration-reference)
- [CLI reference](#cli-reference)
- [Security](#security)
- [Docker](#docker)
- [Building and testing](#building-and-testing)
- [What it does not do yet](#what-it-does-not-do-yet)

## Why

Every MCP client keeps its own list of servers, its own credentials and its own
copy of the same wiring. On one machine that is tolerable. Across a team it is
not: the same server gets added five times, a token is rotated in one place and
forgotten in three, and nobody can answer "what did this thing actually call
yesterday".

wakil-mcp centralises that. It is a proxy, not a framework:

- **One endpoint.** `http://host:8765/mcp`, Streamable HTTP.
- **Add servers in a UI.** Paste a JSON block or fill in a form; test the
  connection before saving; enable and disable per server.
- **Per-harness credentials.** Each harness gets its own bearer token, and a
  profile that can restrict it to a subset of servers and tools.
- **One audit trail.** Every call lands in SQLite with the client, the tool,
  the outcome and the duration, and is readable in the UI.
- **Any upstream.** stdio programs, Streamable HTTP, and the deprecated HTTP+SSE
  transport. Whatever protocol revision the server speaks is negotiated
  per connection, from 2025-06-18 through 2026-07-28.
- **No restart to change anything.** The config file is watched; the CLI and the
  UI both edit it; the daemon re-reads it and republishes the tool list.

## Quick start

Build it and run it. The first start creates the config file and mints two
credentials, printing each once:

```console
$ go build ./cmd/wakil-mcp
$ ./wakil-mcp serve
wakil-mcp dev (none, unknown)
config      /home/you/.config/wakil-mcp/config.json
listening   127.0.0.1:8765
mcp         http://127.0.0.1:8765/mcp
web ui      http://127.0.0.1:8765/

A new config was created at /home/you/.config/wakil-mcp/config.json.

  client token for "laptop", to put in your harnesses:
    wakil_…

  admin token, for the web UI and the REST API:
    wakil_…

Both are shown once; only hashes are stored. Save them now.
```

Keep both. The admin token unlocks the UI; the client token is what a harness
sends. Then open <http://127.0.0.1:8765/>, paste the admin token, and add your
first server under **Servers → Add server**.

From the command line instead:

```console
$ ./wakil-mcp add github --command npx --arg -y --arg @modelcontextprotocol/server-github \
      --env GITHUB_PERSONAL_ACCESS_TOKEN='${env:GITHUB_TOKEN}' --test
$ ./wakil-mcp add notion --url https://mcp.notion.com/mcp \
      --header 'Authorization: Bearer ${env:NOTION_TOKEN}'
```

The daemon picks the change up within a couple of seconds and the tools appear
in every connected harness.

## Everyday use

```console
$ ./wakil-mcp list                     # what the daemon serves
$ ./wakil-mcp test github              # connect once and report what it offers
$ ./wakil-mcp disable github           # take its tools away everywhere
$ ./wakil-mcp enable github
$ ./wakil-mcp token create ci-bot --allow-server github --allow-tool 'github:get_*'
$ ./wakil-mcp doctor                   # is this deployment healthy?
```

## The web UI

The UI is served by the daemon itself, on the same port, and needs the admin
token. It has six tabs:

| Tab | What it is for |
| --- | --- |
| **Servers** | Every configured server: state, tools, pid, last error, stderr. Start, stop, restart, recheck, read the log, enable, disable, remove. **Add server** is a form with a live JSON preview and a *Test connection* button. |
| **Tools** | The exact list a harness sees, with search and a per-server filter. Click a row for the full input schema. Names that had to be renamed or suffixed are called out here. |
| **Harness** | Copy-paste configuration for each harness, with the right secret syntax for each one, plus the stdio bridge command for clients that cannot do HTTP. |
| **Clients** | The bearer tokens a harness uses: which servers and tools each may reach, create, rotate, delete. **New client** lists every configured server with checkboxes (all/none per list and per server), so a scope is ticked rather than typed; hand-written glob patterns still go in an *Advanced* box and are merged on top. A new token is displayed once. |
| **Config** | The raw `config.json` in an editor, with *Validate*, *Format*, *Save and apply*, and a warning when the file changed on disk underneath an unsaved edit. |
| **Activity** | The audit log: one row per call, with client, tool, status, error text and duration; filterable, with retention pruning. |

Everything that changes configuration goes through the same path as the CLI, so
the file, the running daemon, the UI and the CLI never disagree.

## Tool names

An exposed name is `prefix` + separator + the upstream tool's name. The prefix
is the **config key** you chose, not `serverInfo.name` — the specification warns
that `serverInfo.name` is not reliably unique, and you should be able to fix a
name without touching the upstream.

| Situation | Result |
| --- | --- |
| key `github`, tool `create_issue` | `github.create_issue` |
| a character not allowed in a tool name (`A-Za-z0-9_-` and `.` are allowed) | replaced with `_` |
| a name longer than 128 characters | truncated, with `~` + 8 hex digits of the SHA-256 appended |
| two servers wanting the same exposed name | the second gets `-2`, then `-3`, … and the UI reports it |
| a tool the profile may not call | absent from `tools/list` and refused by `tools/call` |

Disambiguation is done once, when the index is built. Routing never parses a
name: every exposed name maps to `{server key, upstream tool name}` in a lookup
table, so an upstream tool with a dot in its name is handled like any other.

Three tools belong to the proxy itself, in a reserved namespace that no upstream
can occupy:

| Tool | Purpose |
| --- | --- |
| `_wakil.search_tools` | find a tool by name, description or server |
| `_wakil.describe_tool` | a tool's full definition, including its schema |
| `_wakil.list_servers` | the servers and their state, filtered to what the caller may reach |

They are useful when the aggregated list is long: the model searches instead of
reading forty tool definitions. `namespace.enabled: false` on a server exposes
its tools without a prefix; a clash with the reserved namespace is suffixed like
any other collision.

## Connecting a harness

Point the harness at the daemon's `/mcp` endpoint and give it a bearer token:

```json
{
  "mcpServers": {
    "wakil": {
      "type": "http",
      "url": "http://127.0.0.1:8765/mcp",
      "headers": { "Authorization": "Bearer wakil_laptop_…" }
    }
  }
}
```

The **Harness** tab generates exactly this, per harness, including the ones that
spell the fields differently (`mcpServers` vs `servers`, `url` vs `serverUrl`).

For a client that only speaks stdio, run the bridge instead of an upstream
server. It forwards each JSON-RPC message to the daemon and writes the reply
back; nothing but protocol traffic goes to stdout:

```json
{
  "mcpServers": {
    "wakil": {
      "command": "wakil-mcp",
      "args": ["stdio"],
      "env": { "WAKIL_TOKEN": "wakil_laptop_…" }
    }
  }
}
```

## Configuration reference

`config.json` is the source of truth. It is created on first run and can be
edited by hand, by the CLI, or in the UI's Config tab — all three take effect
while the daemon runs. Unknown fields are rejected, so a typo is reported rather
than silently ignored. The default location is
`$XDG_CONFIG_HOME/wakil-mcp/config.json` (`%APPDATA%\wakil-mcp\config.json` on
Windows), overridable with `--config` or `WAKIL_CONFIG_DIR`.

```jsonc
{
  "version": 1,

  "server": {
    "bind": "127.0.0.1:8765",     // listen address; 0.0.0.0:8765 to serve a LAN
    "publicUrl": "",              // what the UI shows harnesses, if not the bind
    "dataDir": "",                // where the audit database lives
    "logLevel": "info",           // debug, info, warn, error
    "adminTokenHash": "sha256:…", // required unless disableAuth
    "disableAuth": false,         // true treats every caller as an administrator
    "allowLegacySSE": false,      // also mount the deprecated HTTP+SSE transport
    "statelessHTTP": true,        // session-less Streamable HTTP
    "auditRetentionDays": 30      // 0 keeps records forever
  },

  "defaults": {
    "startPolicy": "lazy",        // eager, lazy or never
    "idleTimeout": "15m",
    "callTimeout": "2m",
    "connectTimeout": "30s",
    "separator": ".",
    "maxToolsPerServer": 0,       // 0 means no limit
    "restartBackoff": { "initial": "1s", "max": "1m", "factor": 2 },
    "namespace": { "enabled": true, "prefix": "", "separator": "" }
  },

  "clients": [
    { "name": "laptop", "tokenHash": "sha256:…" },
    {
      "name": "ci-bot",
      "tokenHash": "sha256:…",
      "allow": { "servers": ["github"], "tools": { "github": ["get_*"] } },
      "deny":  { "tools":    { "github": ["get_secret_*"] } }
    }
  ],

  "mcpServers": {
    "github": {
      "type": "stdio",            // stdio, http, sse, or auto
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "${env:GITHUB_TOKEN}" },
      "cwd": "",
      "description": "Issues and pull requests",
      "enabled": true,
      "startPolicy": "eager",     // per-server override
      "idleTimeout": "30m",
      "namespace": { "prefix": "gh" },
      "tools": {
        "include": ["get_*"],     // patterns match the upstream name
        "exclude": ["delete_*"],  // exclude wins
        "aliases": { "get_issue": "issue" }
      },
      "timeout": "1m"
    },

    "notion": {
      "type": "http",
      "url": "https://mcp.notion.com/mcp",
      "headers": { "Authorization": "Bearer ${env:NOTION_TOKEN}" }
    }
  }
}
```

Notes on the less obvious knobs:

- **`startPolicy`** — `eager` connects at boot and startup waits for it; `lazy`
  connects in the background at boot and on first use, and may be reaped when
  idle; `never` keeps a server configured but unstarted until you start it from
  the UI. Tools of a reaped server stay in the list: calling one restarts it
  transparently.
- **`${env:NAME}` and `${file:path}`** are resolved when a server is started, and
  never appear in the API, the UI or the logs. An unknown scheme is left as
  written, so a literal `${` survives.
- **`separator`** — change it to `__` if your harness dislikes dots in tool
  names. The prefix and the tool name are sanitised separately and joined with
  it, so a multi-character separator survives intact.
- **`namespace.enabled: false`** exposes upstream names as they are, which is
  what you want with a single server and a harness that must see the original
  names.
- **`statelessHTTP`** — leave it on unless you need server-initiated requests.
  Protocol revision 2026-07-28 is only valid without sessions; turning this off
  pins the daemon to the older revisions.

## CLI reference

```
wakil-mcp [global flags] <command> [flags]

  serve     run the proxy daemon
  stdio     relay a stdio harness to a running daemon
  list      list configured servers
  add       add or replace a server
  remove    delete a server
  enable    enable a server
  disable   disable a server
  import    import another tool's mcpServers block
  test      connect to one server and report what it offers
  token     manage client credentials
  doctor    check a deployment for problems
  version   print the build identity

Global flags:
  -c, --config <path>     config file
      --log-level <level> override the configured log level
      --json              machine-readable output where supported
  -v, --version           print the build identity
```

`serve` takes `--bind` and `--no-enumerate` (listen immediately, without waiting
for upstreams). `import` reads the shapes harnesses actually write — an
`mcpServers` map, a VS Code `servers` map, or a bare map of name to server — and
`--dry-run` shows what it would do.

`token create` takes `--allow-server`, `--deny-server`, `--allow-tool S:P` and
`--deny-tool S:P`; patterns accept `*` and `?`, and a tool rule matches either
the exposed name or the upstream name, so either spelling works.

Every command that edits the config is safe to run against a live daemon: the
file is written atomically, and the daemon notices and re-applies it.

## Security

- **Two kinds of credential.** Client tokens reach the MCP endpoint; the admin
  token reaches the REST API and the UI. Only `sha256:` hashes are stored, so a
  token is shown once at creation and cannot be recovered.
- **Profiles.** A client may be limited to named servers and to tool patterns.
  The restriction applies to `tools/list`, to `tools/call` and to the synthetic
  tools, so a narrow token cannot even learn what else exists.
- **Secrets.** Values stay as `${env:…}` or `${file:…}` in the file and are
  resolved when a process is spawned. Snapshots, the API and the logs carry the
  reference, not the value. Error text that could contain a credential is
  withheld from clients that cannot use the server anyway.
- **Bind to localhost by default.** Serving a LAN is a `bind` change and a
  deliberate one. The daemon keeps the SDK's DNS-rebinding protection on.
- **The UI is unauthenticated by design** — a browser has to load the app before
  it can ask for a token. Every API call it makes is authenticated; static
  assets carry no data.
- **Audit records** are written to SQLite in the data directory and pruned after
  `auditRetentionDays`.

What it does not protect: anyone who can read the config file can read the
secret *references*, and anyone who can set environment variables for the daemon
can control what they resolve to. Treat the config file and the daemon's
environment as you would any other credential store.

## Docker

```console
$ docker build -t wakil-mcp .            # podman build --format docker -t wakil-mcp .
$ docker run --rm -p 8765:8765 -v wakil-data:/data \
      -e GITHUB_TOKEN=ghp_… wakil-mcp
```

The image is about 25 MB, builds the web UI itself, and runs as a non-root user.
It serves `0.0.0.0:8765` and keeps its config and database in `/data`. The first
start prints the admin and client tokens to the container log. Mount `/data` if
you want them to survive a restart.

`podman build --format docker` keeps the image's health check; podman's default
OCI output warns and drops it.

Upstream stdio servers have to exist inside the image or on a mount; the image
carries no Node runtime, so the usual `npx` servers are better run by pointing at
an HTTP endpoint or by building your own image `FROM wakil-mcp` with Node added.

### Published images

Every push to `main` publishes `ghcr.io/shabilullah/wakil-mcp` through
`.github/workflows/publish.yml`, as `latest`, `main` and `sha-<commit>`; a `v*`
tag publishes `1.2.3` and `1.2` instead. Pull requests run `go vet`, the Go
tests and the front-end tests, then build the image without pushing it. Images
are built for linux/amd64 and linux/arm64.

`compose.yml` in this repository is a Dockge-ready stack: copy it into a stack
folder and deploy. It pins `latest`, publishes port 8765 (`WAKIL_PORT` moves the
host side) and keeps `/data` in a named volume, so config and tokens survive an
update; Dockge's Update button pulls the newest image.

GHCR packages start out private even when the repository is public. Either make
this package public once under the package settings, or give Dockge a `ghcr.io`
login backed by a token with `read:packages`.

## Building and testing

```console
$ go build ./cmd/wakil-mcp          # the daemon, the CLI and the bridge
$ go test ./...                     # unit, integration and end-to-end tests
$ cd web && npm install && npm run build
$ cd web && npm test                # the front-end helpers, with Node's test runner
```

The built web UI is committed under `internal/server/webui/dist` and embedded
into the binary, so `go build` works without Node. Run `npm run build` in `web/`
after changing the front end; a placeholder `index.html` keeps the embed
satisfied if you delete the build.

The end-to-end tests are the interesting ones: they start a real daemon, add
servers through the CLI, talk to it over Streamable HTTP with the Go SDK, drive
the stdio bridge, and check the served web bundle. `WAKIL_TEST_LOG=1` turns on
debug logging.

No test can tell you what an unrelated MCP implementation thinks of the daemon,
so `interop/probe.mjs` does that separately. It drives the official TypeScript
SDK against a running daemon — listing tools, calling them, checking schemas,
profiles and structured content — and it is what caught a `tools/list` response
that the Go SDK's own client accepted and a strict client rejected. Run it with:

```console
$ cd interop && npm install && node probe.mjs http://127.0.0.1:8765/mcp <client token>
```

## What it does not do yet

- **Tools only.** Resources, prompts, sampling, elicitation and logging are not
  proxied. A server that offers only resources looks empty here.
- **Server-initiated requests are not usable in the default stateless mode**,
  which is what keeps protocol revision 2026-07-28 working. Multi-round-trip
  interactions are passed through untouched, so a harness that supports them
  still sees them.
- **One namespace, no groups.** Every client sees every enabled server unless a
  profile narrows it.
- **No horizontal scaling.** One daemon owns the config file and the audit
  database. Run two on different ports and they are independent.

## Layout

```
cmd/wakil-mcp         the binary: serve, stdio bridge, CLI
cmd/mcpfixture        a test MCP server, stdio, modern or legacy
internal/aggregate    namespacing, collision handling, the lookup table
internal/audit        SQLite audit log
internal/auth         tokens, profiles, the credential registry
internal/bridge       the stdio <-> Streamable HTTP relay
internal/cli          command line, flags, output
internal/config       config schema, validation, atomic writes, watching
internal/harness      per-harness snippet generation
internal/logging      small slog helpers
internal/proxy        the aggregated MCP server and its synthetic tools
internal/secrets      ${env:…} and ${file:…} resolution
internal/server       HTTP surface: MCP endpoint, REST API, web UI
internal/upstream     connecting to, supervising and reaping upstream servers
web/                  the web UI (Vite, React, Tailwind, CodeMirror)
```

## License

MIT. See [LICENSE](LICENSE).
