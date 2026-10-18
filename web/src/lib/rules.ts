// Rule helpers for the client dialogs.
//
// The daemon stores a client's scope as {servers: [pattern], tools:
// {server: [pattern]}}, where every entry is a glob. That is compact but
// unforgiving to type by hand: a misspelled server produces a token that
// silently reaches nothing. These helpers turn the same shape into something a
// checkbox list can drive, and back again.

import type { AllowRule, ServerSnapshot, ToolEntry, UpstreamState } from "../types"

/**
 * RuleState is a rule as the picker holds it: servers granted in full, plus
 * individual tools per server.
 *
 * A server appears in exactly one of the two. In `whole` it is granted without
 * narrowing, so it also covers tools the server publishes later. In `picks` it
 * is granted only for the listed tools.
 */
export interface RuleState {
  whole: string[]
  picks: Record<string, string[]>
}

/** emptyRule is the "no restriction" start state. */
export function emptyRule(): RuleState {
  return { whole: [], picks: {} }
}

/** isEmptyRule reports whether a state places no restriction at all. */
export function isEmptyRule(state: RuleState): boolean {
  return state.whole.length === 0 && Object.values(state.picks).every((list) => list.length === 0)
}

/** pickedTools returns the tools picked under one server. */
export function pickedTools(state: RuleState, key: string): string[] {
  return state.picks[key] ?? []
}

/** ServerInventory is one upstream the picker can offer. */
export interface ServerInventory {
  key: string
  enabled: boolean
  state?: UpstreamState
  lastError?: string
  /** tools holds what can be ticked: the names this proxy exposes when the
   * server is contributing, its own names when it is not. */
  tools: string[]
  /** namesAreUpstream is true when tools holds the server's own names, which
   * only happens while it is not contributing. Rules match either name. */
  namesAreUpstream: boolean
  /** known is false when nothing has been enumerated yet, so an empty tools
   * list means "unknown" rather than "none". */
  known: boolean
}

/**
 * buildInventory merges the server snapshots with the exposed tool index.
 *
 * The index is the better source when a server is contributing, because those
 * are the names the Tools tab shows. A stopped or failing server falls back to
 * the names it reported the last time it was reachable, so its tools can still
 * be picked before it comes back.
 */
export function buildInventory(servers: ServerSnapshot[], tools: ToolEntry[]): ServerInventory[] {
  const exposed = new Map<string, string[]>()
  for (const tool of tools) {
    const list = exposed.get(tool.serverKey)
    if (list) list.push(tool.exposed)
    else exposed.set(tool.serverKey, [tool.exposed])
  }

  const out: ServerInventory[] = []
  const seen = new Set<string>()

  for (const snapshot of servers) {
    seen.add(snapshot.key)

    const names = exposed.get(snapshot.key)
    if (names && names.length > 0) {
      out.push({
        key: snapshot.key,
        enabled: snapshot.enabled,
        state: snapshot.state,
        lastError: snapshot.lastError,
        tools: [...names].sort(),
        namesAreUpstream: false,
        known: true,
      })
      continue
    }

    const own = (snapshot.tools ?? []).map((tool) => tool.name).sort()
    out.push({
      key: snapshot.key,
      enabled: snapshot.enabled,
      state: snapshot.state,
      lastError: snapshot.lastError,
      tools: own,
      namesAreUpstream: true,
      known: own.length > 0,
    })
  }

  // A server that is in the index but has no snapshot should not exist, but
  // hiding a reachable tool would be worse than showing a server twice.
  for (const [key, names] of exposed) {
    if (seen.has(key)) continue
    out.push({
      key,
      enabled: true,
      tools: [...names].sort(),
      namesAreUpstream: false,
      known: true,
    })
  }

  out.sort((a, b) => a.key.localeCompare(b.key))
  return out
}

/**
 * encodeRule turns a picker state into the rule the API stores.
 *
 * The two directions need different shapes from the same ticks:
 *
 *   - An allow rule has to name every server it wants reachable, including the
 *     ones that were only narrowed to a few tools, or the server is refused
 *     before its tools are ever considered.
 *   - A deny rule is the opposite. Naming a server there blocks everything it
 *     publishes, so picking one tool must leave `deny.servers` alone.
 *
 * A server granted in full is encoded as a plain entry in `servers`, and stays
 * that way while the rule has no tool map at all, so it keeps covering tools
 * the server publishes later. Once another server is narrowed, the full grant
 * needs an explicit `"*"` or the map would silently exclude it.
 */
export function encodeRule(state: RuleState, mode: "allow" | "deny"): AllowRule {
  const picks: Record<string, string[]> = {}
  for (const [server, list] of Object.entries(state.picks)) {
    if (list.length > 0) picks[server] = [...list].sort()
  }

  const whole = [...new Set(state.whole)].sort()
  const narrowed = Object.keys(picks)

  const servers = mode === "allow" ? [...new Set([...whole, ...narrowed])].sort() : whole

  const tools: Record<string, string[]> = {}
  if (mode === "allow" && narrowed.length > 0) {
    for (const server of whole) tools[server] = ["*"]
  }
  for (const [server, list] of Object.entries(picks)) tools[server] = list

  return { servers, tools }
}

/** ExtraRule is hand-written glob patterns, added on top of the ticks. */
export interface ExtraRule {
  servers: string[]
  tools: Record<string, string[]>
}

/** parseServers splits the comma or space separated server patterns. */
export function parseServers(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((entry) => entry.trim())
    .filter((entry) => entry !== "")
}

/** ParsedTools is the result of parsing one `server:pattern` per line. */
export interface ParsedTools {
  tools: Record<string, string[]>
  error?: string
}

/** parseToolLines reads one `server:pattern` per line. */
export function parseToolLines(text: string): ParsedTools {
  const tools: Record<string, string[]> = {}

  for (const raw of text.split("\n")) {
    const line = raw.trim()
    if (line === "") continue

    const colon = line.indexOf(":")
    if (colon <= 0) {
      return { tools, error: `"${line}" should be written as server:pattern` }
    }

    const server = line.slice(0, colon).trim()
    const pattern = line.slice(colon + 1).trim()
    if (server === "" || pattern === "") {
      return { tools, error: `"${line}" should be written as server:pattern` }
    }

    const existing = tools[server]
    if (existing) existing.push(pattern)
    else tools[server] = [pattern]
  }

  return { tools }
}

/**
 * mergeExtras adds hand-written patterns to the encoded rule.
 *
 * For an allow rule a server mentioned in the tool map has to appear in
 * `servers` as well, or the rule would grant its tools and then refuse the
 * server they live on. The reverse also holds: *any* tool map switches an allow
 * rule to an explicit allowlist, so servers that were ticked in full need a
 * `"*"` entry or the extra pattern just cut them off. For a deny rule neither
 * applies, and a tool pattern must not promote the server to "entirely
 * blocked".
 */
export function mergeExtras(mode: "allow" | "deny", base: AllowRule, extra: ExtraRule): AllowRule {
  const servers = [...new Set([...(base.servers ?? []), ...extra.servers])]

  const tools: Record<string, string[]> = {}
  for (const [server, patterns] of Object.entries(base.tools ?? {})) {
    tools[server] = [...patterns]
  }
  for (const [server, patterns] of Object.entries(extra.tools)) {
    tools[server] = [...new Set([...(tools[server] ?? []), ...patterns])].sort()
    if (mode === "allow" && !servers.includes(server)) servers.push(server)
  }

  if (mode === "allow" && Object.keys(tools).length > 0) {
    for (const server of servers) {
      if (tools[server] === undefined) tools[server] = ["*"]
    }
  }

  return { servers: servers.sort(), tools }
}

/** describeState summarises a picker state for the dialog header. */
export function describeState(state: RuleState, mode: "allow" | "deny"): string {
  if (isEmptyRule(state)) {
    return mode === "allow" ? "no restriction" : "nothing blocked"
  }

  const tools = Object.values(state.picks).reduce((sum, list) => sum + list.length, 0)

  const parts: string[] = []
  if (state.whole.length > 0) {
    parts.push(`${state.whole.length} ${state.whole.length === 1 ? "server" : "servers"} in full`)
  }
  if (tools > 0) {
    parts.push(`${tools} ${tools === 1 ? "tool" : "tools"}`)
  }
  return parts.join(" + ")
}

/**
 * patternsThatMatchNothing finds literal patterns that match no configured
 * server, which is how a typo turns into a token that reaches nothing.
 */
export function patternsThatMatchNothing(patterns: string[], keys: string[]): string[] {
  return patterns.filter((pattern) => {
    if (pattern.includes("*") || pattern.includes("?")) return false
    return !keys.some((key) => key.toLowerCase() === pattern.toLowerCase())
  })
}
