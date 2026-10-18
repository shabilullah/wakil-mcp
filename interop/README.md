# Interoperability probe

Drives a running wakil-mcp with the **official TypeScript SDK** — a different
implementation of the protocol from the Go SDK the daemon is built on, with a
strict schema validator in front of every response.

That matters. The Go tests exercise the daemon with the Go client, which decodes
responses into typed structs and therefore cannot see a field that should never
have been sent. This probe can. It found `tools/list` responses carrying
`"outputSchema": null`, which the Go client accepted and the TypeScript client
rejected outright — the difference between a harness working and a harness
reporting a protocol error.

## Running it

Start a daemon, mint a token, and point the probe at it:

```console
$ wakil-mcp serve &
$ cd interop && npm install

$ node probe.mjs http://127.0.0.1:8765/mcp <client-token>
server:   wakil-mcp dev
protocol: 2025-11-25
list changed notifications: true

PASS  tools/list arrives and is not empty — 12 tools
...
SKIP  profile checks (set WAKIL_BOT_TOKEN to a narrower client token)

10/10 checks passed
```

Pass a second, narrower client token in `WAKIL_BOT_TOKEN` to include the
profile checks (that a restricted token sees only its own tools, that the server
listing is filtered to it, and that a hidden tool is refused):

```console
$ WAKIL_BOT_TOKEN=wakil_readonly_… node probe.mjs http://127.0.0.1:8765/mcp wakil_laptop_…
```

Without it those checks are reported as skipped. The exit code is 0 when every
check passed and 1 otherwise, so it drops into a pipeline as-is.

## What it covers

- `tools/list` shape: namespaced names, descriptions, input schemas that are
  objects and never null, and the proxy's own `_wakil.*` tools
- `tools/call`: a real upstream call, structured content, and a tool-level error
  staying a result rather than becoming a protocol error
- error handling: an unknown tool, and a tool outside the caller's profile
- profiles: a narrowed roster, a filtered server listing, and the reported counts
