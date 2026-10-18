import type {
  AuditResponse,
  ClientView,
  ConfigResponse,
  CreateClientResponse,
  HarnessResponse,
  ServerSnapshot,
  Status,
  TestResult,
  ToolEntry,
  ValidateResponse,
  Collision,
  AllowRule,
  UpstreamConfig,
} from "./types"

/** ApiError carries the daemon's structured error body. */
export class ApiError extends Error {
  readonly status: number
  readonly detail: string[]

  constructor(status: number, message: string, detail: string[] = []) {
    super(message)
    this.name = "ApiError"
    this.status = status
    this.detail = detail
  }

  /** True when the credential was rejected, as opposed to the request being wrong. */
  get isAuthFailure(): boolean {
    return this.status === 401 || this.status === 403
  }
}

/**
 * Client is a thin wrapper over the daemon's REST API.
 *
 * Every call attaches the admin token, because the whole surface is behind it
 * and there is no unauthenticated endpoint worth reaching.
 */
export class Client {
  private readonly token: string

  constructor(token: string) {
    this.token = token
  }

  private async call<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { Accept: "application/json" }
    if (body !== undefined) {
      headers["Content-Type"] = "application/json"
    }
    if (this.token !== "") {
      headers.Authorization = `Bearer ${this.token}`
    }

    let resp: Response
    try {
      resp = await fetch(path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
      })
    } catch (err) {
      throw new ApiError(0, `the daemon is not reachable: ${(err as Error).message}`)
    }

    const text = await resp.text()
    let parsed: unknown
    if (text.trim() !== "") {
      try {
        parsed = JSON.parse(text)
      } catch {
        // A non-JSON body means something upstream of the API answered.
        if (!resp.ok) {
          throw new ApiError(resp.status, text.trim().slice(0, 300))
        }
        throw new ApiError(resp.status, "the daemon returned a body that is not JSON")
      }
    }

    if (!resp.ok) {
      const failure = parsed as { error?: string; detail?: string[] } | undefined
      throw new ApiError(
        resp.status,
        failure?.error ?? `${method} ${path} failed with ${resp.status}`,
        failure?.detail ?? [],
      )
    }

    return parsed as T
  }

  // ---------------------------------------------------------------- status

  status(): Promise<Status> {
    return this.call<Status>("GET", "/api/status")
  }

  // --------------------------------------------------------------- config

  config(): Promise<ConfigResponse> {
    return this.call<ConfigResponse>("GET", "/api/config")
  }

  saveConfig(json: string): Promise<{ ok: boolean }> {
    return this.call<{ ok: boolean }>("PUT", "/api/config", { json })
  }

  validateConfig(json: string): Promise<ValidateResponse> {
    return this.call<ValidateResponse>("POST", "/api/config/validate", { json })
  }

  // -------------------------------------------------------------- servers

  async servers(): Promise<ServerSnapshot[]> {
    const res = await this.call<{ servers: ServerSnapshot[] | null }>("GET", "/api/servers")
    return res.servers ?? []
  }

  server(key: string): Promise<ServerSnapshot> {
    return this.call<ServerSnapshot>("GET", `/api/servers/${encodeURIComponent(key)}`)
  }

  startServer(key: string): Promise<ServerSnapshot> {
    return this.call<ServerSnapshot>("POST", `/api/servers/${encodeURIComponent(key)}/start`)
  }

  stopServer(key: string): Promise<ServerSnapshot> {
    return this.call<ServerSnapshot>("POST", `/api/servers/${encodeURIComponent(key)}/stop`)
  }

  restartServer(key: string): Promise<ServerSnapshot> {
    return this.call<ServerSnapshot>("POST", `/api/servers/${encodeURIComponent(key)}/restart`)
  }

  async serverStderr(key: string): Promise<string> {
    const res = await this.call<{ stderr: string }>(
      "GET",
      `/api/servers/${encodeURIComponent(key)}/stderr`,
    )
    return res.stderr
  }

  testServer(server: UpstreamConfig, key = "test"): Promise<TestResult> {
    return this.call<TestResult>("POST", "/api/servers/test", { key, server })
  }

  // ---------------------------------------------------------------- tools

  async tools(params: { server?: string; q?: string } = {}): Promise<{
    tools: ToolEntry[]
    count: number
    total: number
    servers: string[]
    collisions: Collision[]
  }> {
    const query = new URLSearchParams()
    if (params.server) query.set("server", params.server)
    if (params.q) query.set("q", params.q)
    const suffix = query.toString() === "" ? "" : `?${query}`

    const res = await this.call<{
      tools: ToolEntry[] | null
      count: number
      total: number
      servers: string[] | null
      collisions: Collision[] | null
    }>("GET", `/api/tools${suffix}`)

    return {
      tools: res.tools ?? [],
      count: res.count,
      total: res.total,
      servers: res.servers ?? [],
      collisions: res.collisions ?? [],
    }
  }

  // -------------------------------------------------------------- clients

  async clients(): Promise<ClientView[]> {
    const res = await this.call<{ clients: ClientView[] | null }>("GET", "/api/clients")
    return res.clients ?? []
  }

  createClient(name: string, allow: AllowRule, deny: AllowRule): Promise<CreateClientResponse> {
    return this.call<CreateClientResponse>("POST", "/api/clients", { name, allow, deny })
  }

  deleteClient(name: string): Promise<{ ok: boolean }> {
    return this.call<{ ok: boolean }>("DELETE", `/api/clients/${encodeURIComponent(name)}`)
  }

  rotateClient(name: string): Promise<CreateClientResponse> {
    return this.call<CreateClientResponse>(
      "POST",
      `/api/clients/${encodeURIComponent(name)}/rotate`,
    )
  }

  setClientEnabled(name: string, enabled: boolean): Promise<ClientView> {
    return this.call<ClientView>(
      "POST",
      `/api/clients/${encodeURIComponent(name)}/enabled`,
      { enabled },
    )
  }

  // ---------------------------------------------------------------- audit

  audit(params: { status?: string; server?: string; client?: string; limit?: number } = {}) {
    const query = new URLSearchParams()
    if (params.status) query.set("status", params.status)
    if (params.server) query.set("server", params.server)
    if (params.client) query.set("client", params.client)
    query.set("limit", String(params.limit ?? 100))
    return this.call<AuditResponse>("GET", `/api/audit?${query}`)
  }

  pruneAudit(days: number): Promise<{ removed: number; days: number }> {
    return this.call<{ removed: number; days: number }>("POST", "/api/audit/prune", { days })
  }

  // -------------------------------------------------------------- harness

  async harnesses(): Promise<string[]> {
    const res = await this.call<{ clients: string[] | null }>("GET", "/api/harness")
    return res.clients ?? []
  }

  harness(client: string): Promise<HarnessResponse> {
    return this.call<HarnessResponse>("GET", `/api/harness/${encodeURIComponent(client)}`)
  }
}
