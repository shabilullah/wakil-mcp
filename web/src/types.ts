// Types mirroring the daemon's JSON. They are written by hand rather than
// generated, because the API surface is small and the field names here are the
// contract the UI depends on.

export interface ApiErrorBody {
  error: string
  detail?: string[]
}

export type UpstreamState = "disabled" | "stopped" | "starting" | "ready" | "error"

export interface ToolAnnotations {
  title?: string
  readOnlyHint?: boolean
  destructiveHint?: boolean
  idempotentHint?: boolean
  openWorldHint?: boolean
}

export interface UpstreamTool {
  name: string
  title?: string
  description?: string
  inputSchema?: unknown
  outputSchema?: unknown
  annotations?: ToolAnnotations
}

export interface UpstreamConfig {
  description?: string
  enabled?: boolean
  type?: string
  command?: string
  args?: string[]
  env?: Record<string, string>
  cwd?: string
  url?: string
  headers?: Record<string, string>
  startPolicy?: string
  connectTimeout?: string
  callTimeout?: string
  idleTimeout?: string
  maxToolsPerServer?: number
  legacySSE?: boolean
  toolAliases?: Record<string, string>
  includeTools?: string[]
  excludeTools?: string[]
}

export interface ServerSnapshot {
  key: string
  enabled: boolean
  state: UpstreamState
  transport?: string
  endpoint?: string
  startPolicy?: string
  idleTimeout?: string
  lastError?: string
  failureCount: number
  nextAttemptAt?: string
  connectedAt?: string
  lastUsedAt?: string
  pid?: number
  inFlight: number
  protocolVersion?: string
  legacy?: boolean
  serverName?: string
  serverVersion?: string
  instructions?: string
  capabilities?: Record<string, unknown>
  tools?: UpstreamTool[]
  toolCount: number
  stderr?: string
  config?: UpstreamConfig
}

export interface ToolEntry {
  exposed: string
  serverKey: string
  upstreamName: string
  tool: UpstreamTool
  truncated?: boolean
  renamed?: boolean
  disambiguated?: boolean
}

export interface Collision {
  serverKey: string
  wanted: string
  given: string
  heldBy: string
}

export interface Status {
  version: string
  publicUrl: string
  mcpEndpoint: string
  dataDir: string
  authEnabled: boolean
  serverCount: number
  states: Record<string, number>
  toolCount: number
  clientCount: number
  collisions: Collision[] | null
}

export interface ConfigResponse {
  path: string
  version: number
  json: string
}

export interface ValidateResponse {
  valid: boolean
  problems: string[] | null
}

export interface AllowRule {
  servers?: string[]
  tools?: Record<string, string[]>
}

export interface ClientView {
  name: string
  hasToken: boolean
  tokenHint?: string
  disabled: boolean
  allow: AllowRule
  deny: AllowRule
  visibleTools: number
}

export interface AllowedRule {
  servers?: string[]
  tools?: Record<string, string[]>
}

export interface CreateClientResponse {
  client: ClientView
  token: string
  note?: string
}

export interface TestSuccess {
  ok: true
  info: {
    protocolVersion: string
    legacy: boolean
    serverInfo?: { name?: string; version?: string; title?: string }
    capabilities?: Record<string, unknown>
    instructions?: string
    tools: UpstreamTool[]
    transport: string
    endpoint?: string
    connectedAt: string
  }
}

export interface TestFailure {
  ok: false
  error: string
}

export type TestResult = TestSuccess | TestFailure

export interface Snippet {
  label: string
  language: string
  filename?: string
  content: string
  note?: string
  deprecated?: boolean
}

export interface HarnessResponse {
  client: string
  baseUrl: string
  snippets: Snippet[]
}

export interface AuditEvent {
  id: number
  time: string
  client: string
  server: string
  tool: string
  upstreamTool?: string
  status: string
  error?: string
  durationMs: number
}

export interface AuditStats {
  total: number
  byStatus: Record<string, number>
  byServer: Record<string, number>
  dropped: number
  oldestAt?: string
  newestAt?: string
}

export interface AuditResponse {
  events: AuditEvent[] | null
  stats: AuditStats
  enabled: boolean
}
