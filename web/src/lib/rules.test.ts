// Tests for the client rule helpers, run with Node's own test runner:
//
//     npm test        (node --test, no extra dependencies)
//
// The part worth pinning down is the difference between the two directions: an
// allow rule has to name every server it wants reachable, while a deny rule
// must not, because naming a server there blocks everything it publishes.
// Getting that backwards silently denies far more than was picked, so the
// daemon's matching rules are mirrored below and every encoding is checked
// against the traffic it actually produces.

import assert from "node:assert/strict"
import { test } from "node:test"

import {
  buildInventory,
  describeState,
  emptyRule,
  encodeRule,
  mergeExtras,
  parseServers,
  parseToolLines,
  patternsThatMatchNothing,
  type RuleState,
} from "./rules.ts"
import type { AllowRule, ServerSnapshot, ToolEntry, UpstreamState, UpstreamTool } from "../types.ts"

/* -------------------------------------------------------------------------- */
/* A mirror of internal/auth/auth.go                                           */
/* -------------------------------------------------------------------------- */

/** matchPattern mirrors the daemon's glob: * for any run, ? for one, fold case. */
function matchPattern(pattern: string, name: string): boolean {
  if (pattern === "") return false
  if (pattern === "*") return true
  if (!pattern.includes("*") && !pattern.includes("?")) {
    return pattern.toLowerCase() === name.toLowerCase()
  }
  const escaped = pattern.toLowerCase().replace(/[.+^${}()|[\]\\]/g, "\\$&")
  const source = `^${escaped.replace(/\*/g, ".*").replace(/\?/g, ".")}$`
  return new RegExp(source).test(name.toLowerCase())
}

function lookupServerPatterns(
  patterns: Record<string, string[]> | undefined,
  serverKey: string,
): string[] | undefined {
  if (!patterns) return undefined

  const exact = patterns[serverKey]
  if (exact !== undefined) return exact

  for (const [key, list] of Object.entries(patterns)) {
    if (key === "*" || matchPattern(key, serverKey)) return list
  }
  return undefined
}

function allowsServer(allow: AllowRule, deny: AllowRule, serverKey: string): boolean {
  const denied = deny.servers ?? []
  if (denied.length > 0 && denied.some((pattern) => matchPattern(pattern, serverKey))) return false

  const granted = allow.servers ?? []
  if (granted.length === 0) return true
  return granted.some((pattern) => matchPattern(pattern, serverKey))
}

/** allowsTool answers what the proxy would answer for one named tool. */
function allowsTool(
  allow: AllowRule,
  deny: AllowRule,
  serverKey: string,
  exposed: string,
  upstream: string,
): boolean {
  if (!allowsServer(allow, deny, serverKey)) return false

  const denied = lookupServerPatterns(deny.tools, serverKey)
  if (denied?.some((pattern) => matchPattern(pattern, exposed) || matchPattern(pattern, upstream))) {
    return false
  }

  if (Object.keys(allow.tools ?? {}).length === 0) return true

  const granted = lookupServerPatterns(allow.tools, serverKey)
  if (granted === undefined) return false
  return granted.some((pattern) => matchPattern(pattern, exposed) || matchPattern(pattern, upstream))
}

/* -------------------------------------------------------------------------- */
/* Fixtures                                                                    */
/* -------------------------------------------------------------------------- */

function state(whole: string[], picks: Record<string, string[]>): RuleState {
  return { whole, picks }
}

function snapshot(
  key: string,
  over: { state?: UpstreamState; lastError?: string; tools?: UpstreamTool[] } = {},
): ServerSnapshot {
  return {
    key,
    enabled: true,
    state: over.state ?? "ready",
    lastError: over.lastError,
    tools: over.tools,
    failureCount: 0,
    inFlight: 0,
    toolCount: over.tools?.length ?? 0,
  }
}

function entry(serverKey: string, exposed: string, upstreamName: string): ToolEntry {
  return { exposed, serverKey, upstreamName, tool: { name: upstreamName } }
}

/* -------------------------------------------------------------------------- */
/* Encoding                                                                    */
/* -------------------------------------------------------------------------- */

test("an empty rule places no restriction", () => {
  assert.deepEqual(encodeRule(emptyRule(), "allow"), { servers: [], tools: {} })
  assert.deepEqual(encodeRule(emptyRule(), "deny"), { servers: [], tools: {} })
})

test("a server ticked in full is named and nothing else", () => {
  // No tool map, so the server's entry already covers tools it publishes later.
  assert.deepEqual(encodeRule(state(["github"], {}), "allow"), {
    servers: ["github"],
    tools: {},
  })
})

test("a narrowed server is named and gets its tools listed", () => {
  const encoded = encodeRule(state([], { github: ["get_issue", "list_issues"] }), "allow")
  assert.deepEqual(encoded, {
    servers: ["github"],
    tools: { github: ["get_issue", "list_issues"] },
  })
})

test("a full grant keeps its coverage when another server is narrowed", () => {
  // The tool map exists now, so github needs "*" or the map would exclude it.
  const encoded = encodeRule(state(["github"], { linear: ["list_teams"] }), "allow")
  assert.deepEqual(encoded, {
    servers: ["github", "linear"],
    tools: { github: ["*"], linear: ["list_teams"] },
  })
})

test("a deny rule never promotes a tool pick to a whole server", () => {
  // This is the bug this test exists for: listing the server under deny.servers
  // blocks every tool it publishes, not just the one that was picked.
  const encoded = encodeRule(state([], { github: ["delete_repo"] }), "deny")
  assert.deepEqual(encoded, {
    servers: [],
    tools: { github: ["delete_repo"] },
  })
})

test("a deny rule names whole servers and leaves tool picks alone", () => {
  const encoded = encodeRule(state(["experiments"], { github: ["delete_repo"] }), "deny")
  assert.deepEqual(encoded, {
    servers: ["experiments"],
    tools: { github: ["delete_repo"] },
  })
})

test("encoded names come back sorted and deduplicated", () => {
  const encoded = encodeRule(state(["zeta", "alpha", "zeta"], { beta: ["b", "a"], gamma: [] }), "allow")
  assert.deepEqual(encoded.servers, ["alpha", "beta", "zeta"])
  assert.deepEqual(encoded.tools, { alpha: ["*"], zeta: ["*"], beta: ["a", "b"] })
})

/* -------------------------------------------------------------------------- */
/* Meaning: what the daemon would allow                                        */
/* -------------------------------------------------------------------------- */

test("picking one tool allows that tool and nothing else on the server", () => {
  const allow = encodeRule(state([], { legacy: ["legacy.add"] }), "allow")

  assert.equal(allowsTool(allow, {}, "legacy", "legacy.add", "add"), true)
  assert.equal(allowsTool(allow, {}, "legacy", "legacy.echo", "echo"), false)
  // A server that was not picked at all stops being reachable.
  assert.equal(allowsTool(allow, {}, "modern", "modern.add", "add"), false)
})

test("a full grant covers tools that do not exist yet", () => {
  const allow = encodeRule(state(["legacy"], { modern: ["modern.echo"] }), "allow")

  assert.equal(allowsTool(allow, {}, "legacy", "legacy.brand_new", "brand_new"), true)
  assert.equal(allowsTool(allow, {}, "modern", "modern.echo", "echo"), true)
  assert.equal(allowsTool(allow, {}, "modern", "modern.add", "add"), false)
})

test("denying one tool leaves the rest of its server reachable", () => {
  const deny = encodeRule(state([], { legacy: ["legacy.error"] }), "deny")

  assert.equal(allowsTool({}, deny, "legacy", "legacy.error", "error"), false)
  assert.equal(allowsTool({}, deny, "legacy", "legacy.add", "add"), true)
})

test("denying a whole server overrides an allow that named its tools", () => {
  const allow = encodeRule(state([], { legacy: ["legacy.add"] }), "allow")
  const deny = encodeRule(state(["legacy"], {}), "deny")

  assert.equal(allowsTool(allow, deny, "legacy", "legacy.add", "add"), false)
})

test("a deny beats an allow for the same tool", () => {
  const allow = encodeRule(state(["legacy"], {}), "allow")
  const deny = encodeRule(state([], { legacy: ["legacy.add"] }), "deny")

  assert.equal(allowsTool(allow, deny, "legacy", "legacy.add", "add"), false)
  assert.equal(allowsTool(allow, deny, "legacy", "legacy.echo", "echo"), true)
})

/* -------------------------------------------------------------------------- */
/* Hand-written patterns                                                       */
/* -------------------------------------------------------------------------- */

test("extra allow patterns are added on top of the ticks", () => {
  const merged = mergeExtras("allow", encodeRule(state(["legacy"], {}), "allow"), {
    servers: ["lin*"],
    tools: { github: ["get_*"] },
  })

  // github has to be reachable for its pattern to mean anything, and the
  // servers ticked in full have to be restated now that a tool map exists.
  assert.deepEqual(merged.servers, ["github", "legacy", "lin*"])
  assert.deepEqual(merged.tools, { github: ["get_*"], legacy: ["*"], "lin*": ["*"] })
  assert.equal(allowsTool(merged, {}, "linear", "linear.list_teams", "list_teams"), true)
  assert.equal(allowsTool(merged, {}, "legacy", "legacy.add", "add"), true)
  assert.equal(allowsTool(merged, {}, "github", "github.get_issue", "get_issue"), true)
  assert.equal(allowsTool(merged, {}, "github", "github.delete_repo", "delete_repo"), false)
  assert.equal(allowsTool(merged, {}, "modern", "modern.add", "add"), false)
})

test("extra deny patterns do not block a whole server", () => {
  const merged = mergeExtras("deny", encodeRule(emptyRule(), "deny"), {
    servers: [],
    tools: { github: ["delete_*"] },
  })

  assert.deepEqual(merged.servers, [])
  assert.equal(allowsTool({}, merged, "github", "github.delete_repo", "delete_repo"), false)
  assert.equal(allowsTool({}, merged, "github", "github.get_issue", "get_issue"), true)
})

test("an extra pattern does not cut off the servers that were ticked in full", () => {
  // Any tool map switches an allow rule to an explicit allowlist, so the full
  // grant has to be re-stated as "*" or the extra pattern silently removes it.
  const merged = mergeExtras("allow", encodeRule(state(["legacy"], {}), "allow"), {
    servers: [],
    tools: { github: ["get_*"] },
  })

  assert.deepEqual(merged.servers, ["github", "legacy"])
  assert.deepEqual(merged.tools, { github: ["get_*"], legacy: ["*"] })
  assert.equal(allowsTool(merged, {}, "legacy", "legacy.add", "add"), true)
  assert.equal(allowsTool(merged, {}, "github", "github.get_issue", "get_issue"), true)
  assert.equal(allowsTool(merged, {}, "github", "github.delete_repo", "delete_repo"), false)
})

test("an extra pattern that is already ticked is not written twice", () => {
  const merged = mergeExtras("allow", encodeRule(state([], { legacy: ["legacy.add"] }), "allow"), {
    servers: [],
    tools: { legacy: ["legacy.add"] },
  })

  assert.deepEqual(merged.tools, { legacy: ["legacy.add"] })
  assert.deepEqual(merged.servers, ["legacy"])
})

test("servers parse from commas, spaces, or both", () => {
  assert.deepEqual(parseServers("github, linear"), ["github", "linear"])
  assert.deepEqual(parseServers("github  linear\nzeta"), ["github", "linear", "zeta"])
  assert.deepEqual(parseServers("  "), [])
})

test("tool lines parse as server:pattern, and complain when they do not", () => {
  const ok = parseToolLines("github:get_*\nlinear:list_*")
  assert.equal(ok.error, undefined)
  assert.deepEqual(ok.tools, { github: ["get_*"], linear: ["list_*"] })

  assert.match(parseToolLines("github").error ?? "", /server:pattern/)
  assert.match(parseToolLines("github:").error ?? "", /server:pattern/)
  assert.match(parseToolLines(":get_*").error ?? "", /server:pattern/)
})

test("patterns that match no server are reported, globs are not", () => {
  assert.deepEqual(patternsThatMatchNothing(["legacy", "githb", "lin*", "LEGACY"], ["legacy", "linear"]), [
    "githb",
  ])
})

/* -------------------------------------------------------------------------- */
/* Inventory                                                                   */
/* -------------------------------------------------------------------------- */

test("the inventory prefers the names this proxy exposes", () => {
  const inventory = buildInventory(
    [snapshot("legacy")],
    [entry("legacy", "legacy.add", "add"), entry("legacy", "legacy.echo", "echo")],
  )

  assert.equal(inventory.length, 1)
  assert.deepEqual(inventory[0]?.tools, ["legacy.add", "legacy.echo"])
  assert.equal(inventory[0]?.namesAreUpstream, false)
  assert.equal(inventory[0]?.known, true)
})

test("a server that is not contributing falls back to its own names", () => {
  const inventory = buildInventory(
    [
      snapshot("offline", {
        state: "stopped",
        tools: [{ name: "add" }, { name: "echo" }],
      }),
    ],
    [],
  )

  assert.deepEqual(inventory[0]?.tools, ["add", "echo"])
  assert.equal(inventory[0]?.namesAreUpstream, true)
  assert.equal(inventory[0]?.known, true)
})

test("a server that never enumerated is marked unknown rather than empty", () => {
  const inventory = buildInventory([snapshot("flaky", { state: "error", lastError: "boom" })], [])

  assert.deepEqual(inventory[0]?.tools, [])
  assert.equal(inventory[0]?.known, false)
  assert.equal(inventory[0]?.state, "error")
  assert.equal(inventory[0]?.lastError, "boom")
})

test("a tool in the index with no snapshot is still offered", () => {
  const inventory = buildInventory([], [entry("ghost", "ghost.x", "x")])

  assert.equal(inventory.length, 1)
  assert.equal(inventory[0]?.key, "ghost")
  assert.deepEqual(inventory[0]?.tools, ["ghost.x"])
})

test("the inventory is sorted by key", () => {
  const inventory = buildInventory([snapshot("zeta"), snapshot("alpha")], [])
  assert.deepEqual(
    inventory.map((server) => server.key),
    ["alpha", "zeta"],
  )
})

/* -------------------------------------------------------------------------- */
/* Summary text                                                                */
/* -------------------------------------------------------------------------- */

test("the summary reads as a sentence about the selection", () => {
  assert.equal(describeState(emptyRule(), "allow"), "no restriction")
  assert.equal(describeState(emptyRule(), "deny"), "nothing blocked")
  assert.equal(describeState(state(["legacy"], {}), "allow"), "1 server in full")
  assert.equal(describeState(state(["a", "b"], { legacy: ["legacy.add"] }), "allow"), "2 servers in full + 1 tool")
})
