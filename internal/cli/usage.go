package cli

// Usage text for each command. Kept together so the help output can be read as
// one document rather than hunted for across files.

const serveUsage = `Usage: wakil-mcp serve [flags]

Runs the proxy. Harnesses connect to the MCP endpoint it prints on start-up.

Flags:
  --bind <host:port>   override server.bind from the config
  --no-enumerate       listen immediately, without connecting to upstreams
  -h, --help           show this help

The process creates the config file with a client token and an admin token the
first time it runs, prints both once, and then serves.
`

const stdioUsage = `Usage: wakil-mcp stdio [flags]

Relays a stdio-only harness to a running daemon.

Point the harness at "wakil-mcp stdio" instead of at an upstream server. Each
JSON-RPC message the harness writes to stdin is forwarded to the daemon, and the
reply is written back to stdout.

Flags:
  --url <url>                 daemon endpoint (default: /mcp of the config's bind)
  --token <token>             bearer token (default: $WAKIL_TOKEN)
  --protocol-version <rev>    pin a protocol revision
  --poll <duration>           how often to check for tool-list changes (default 15s)
  -h, --help                  show this help

Nothing but protocol traffic is written to stdout, so the bridge is safe to use
where the harness parses that stream.
`

const listUsage = `Usage: wakil-mcp list [--json]

Lists the servers in the config file. This is what the daemon will serve on its
next start; use "wakil-mcp test <key>" to check one right now.

Flags:
  --json        machine-readable output
  -h, --help    show this help
`

const addUsage = `Usage: wakil-mcp add <key> (--command <exe> | --url <url>) [flags]

Adds a server, or replaces one that already has the same key.

Flags:
  --command <exe>          stdio: the program to run
  --arg <value>            stdio: an argument (repeatable)
  --env KEY=VALUE          stdio: an environment variable (repeatable)
  --cwd <dir>              stdio: working directory
  --url <url>              http or sse: the endpoint
  --header KEY=VALUE       http: a request header (repeatable, "Key: Value" also works)
  --type <kind>            stdio, http, sse or auto (inferred by default)
  --description <text>     free text shown in the UI
  --start-policy <policy>  lazy, eager or never
  --disabled               add the server switched off
  --test                   connect once and report what the server offers
  -h, --help               show this help

Values may reference secrets as ${env:NAME} or ${file:path}; both are resolved
when the server is started, never written out in API responses.

Examples:
  wakil-mcp add github --command npx --arg -y --arg @modelcontextprotocol/server-github \
      --env GITHUB_PERSONAL_ACCESS_TOKEN='${env:GITHUB_TOKEN}'
  wakil-mcp add linear --url https://mcp.linear.app/mcp --header "Authorization: Bearer ${env:LINEAR_KEY}"
`

const removeUsage = `Usage: wakil-mcp remove <key>

Deletes a server from the config. Nothing else changes, so re-adding the same
key restores it.
`

const toggleUsage = `Usage: wakil-mcp <enable|disable> <key>

Enables or disables a server without deleting its configuration. A disabled
server contributes no tools and is never started.
`

const importUsage = `Usage: wakil-mcp import --from <file> [flags]

Reads another tool's MCP configuration and copies its servers in.

Flags:
  --from <file>    the configuration to read
  --prefix <text>  prefix each imported key, to avoid collisions
  --dry-run        report what would be added without saving
  -h, --help       show this help

Accepts the shapes harnesses actually write: an "mcpServers" map, a VS Code
"servers" map, or a bare map of name to server. Unknown keys are ignored, so a
newer harness configuration still imports.

Examples:
  wakil-mcp import --from ~/.cursor/mcp.json
  wakil-mcp import --from ./claude_desktop_config.json --prefix old-
`

const testUsage = `Usage: wakil-mcp test <key> [flags]

Connects to one server, reports what it offers, and disconnects. This is the
same check the UI's test button runs.

Flags:
  --timeout <duration>   how long to allow for the connection (default 30s)
  --json                 machine-readable output
  -h, --help             show this help
`

const tokenUsage = `Usage: wakil-mcp token <subcommand> [flags]

Manages the credentials harnesses and operators use.

Subcommands:
  create <name>   mint a token for a harness
  list            show the configured profiles
  remove <name>   delete a profile
  admin           mint a new admin token, replacing the current one

Flags for "create":
  --allow-server <pattern>   servers the token may reach (repeatable)
  --deny-server <pattern>    servers to exclude (repeatable)
  --allow-tool <S:P>         tools the token may call, as SERVER:PATTERN (repeatable)
  --deny-tool <S:P>          tools to exclude, as SERVER:PATTERN (repeatable)
  --json                     machine-readable output

A pattern may use * and ? wildcards. A tool rule matches either the exposed name
("github.get_issue") or the upstream name, so either spelling works.

A token is shown once, when it is created. Only its hash is stored, so a lost
token has to be replaced rather than looked up.

Examples:
  wakil-mcp token create laptop
  wakil-mcp token create ci --allow-server github --allow-tool "github:get_*"
  wakil-mcp token create readonly --allow-server "gh-*" --deny-tool "github:delete_*"
`

const doctorUsage = `Usage: wakil-mcp doctor [flags]

Checks a deployment and reports what is wrong with it.

Flags:
  --skip-servers        do not connect to the servers, only check the environment
  --timeout <duration>  per-server connection timeout (default 30s)
  --json                machine-readable output
  -h, --help            show this help

The environment checks cover the config file, the data directory, the audit
database and the bind address. The server checks load each configured server,
report its tools, and disconnect.
`

const versionUsage = `Usage: wakil-mcp version [--json]

Prints the version, commit and build date.
`
