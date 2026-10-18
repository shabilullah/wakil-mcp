import { useMemo, useState } from "react"
import { useApp } from "../app-context"
import { ApiError } from "../api"
import { useToast } from "./Toast"
import { Badge, Code, ErrorNote, Field, Modal, Spinner } from "./ui"
import { serversOf } from "../lib/config"
import type { TestResult, UpstreamConfig } from "../types"

/** keyPattern mirrors the daemon's rule for config keys. */
const keyPattern = /^[A-Za-z0-9_-]{1,48}$/
const reserved = "_wakil"

type Transport = "stdio" | "http" | "sse" | "auto"

/**
 * splitLines turns a textarea into a list, ignoring blank lines.
 * Trailing whitespace is kept out but inner spaces are preserved, so an
 * argument list survives intact.
 */
function splitLines(text: string): string[] {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "")
}

/** parsePairs reads "KEY=VALUE" or "Key: Value" lines into a map. */
function parsePairs(text: string): { values: Record<string, string>; problem?: string } {
  const values: Record<string, string> = {}

  for (const raw of text.split("\n")) {
    const line = raw.trim()
    if (line === "") continue

    let key: string | undefined
    let value: string | undefined

    const equals = line.indexOf("=")
    if (equals >= 0) {
      key = line.slice(0, equals).trim()
      value = line.slice(equals + 1)
    } else {
      const colon = line.indexOf(":")
      if (colon >= 0) {
        key = line.slice(0, colon).trim()
        value = line.slice(colon + 1).trim()
      }
    }

    if (key === undefined || key === "") {
      return { values, problem: `"${line}" is not KEY=VALUE or Key: Value` }
    }
    values[key] = value ?? ""
  }

  return { values }
}

/**
 * AddServerDialog builds one mcpServers entry and writes it through the shared
 * configuration, so the raw editor and this form never disagree.
 */
export function AddServerDialog({
  open,
  onClose,
  onAdded,
}: {
  open: boolean
  onClose: () => void
  onAdded?: (key: string) => void
}) {
  const { client, config, bump } = useApp()
  const toast = useToast()

  const [key, setKey] = useState("")
  const [transport, setTransport] = useState<Transport>("stdio")
  const [description, setDescription] = useState("")

  const [command, setCommand] = useState("")
  const [args, setArgs] = useState("")
  const [cwd, setCwd] = useState("")
  const [env, setEnv] = useState("")

  const [url, setUrl] = useState("")
  const [headers, setHeaders] = useState("")

  const [startPolicy, setStartPolicy] = useState("")
  const [idleTimeout, setIdleTimeout] = useState("")
  const [maxTools, setMaxTools] = useState("")
  const [advanced, setAdvanced] = useState(false)

  const [result, setResult] = useState<TestResult | null>(null)
  const [busy, setBusy] = useState<"test" | "add" | null>(null)
  const [error, setError] = useState<unknown>(null)

  const keyProblem = useMemo(() => {
    if (key === "") return "a key is required"
    if (!keyPattern.test(key)) return "use 1-48 letters, digits, underscores or dashes"
    if (key === reserved || key.startsWith(`${reserved}_`) || key.startsWith(`${reserved}-`)) {
      return `keys beginning with ${reserved} are reserved for the proxy itself`
    }
    if (config.text !== "" && !config.dirty) {
      try {
        const doc = JSON.parse(config.text) as { mcpServers?: Record<string, unknown> }
        if (doc.mcpServers && key in doc.mcpServers) return `a server named ${key} already exists; adding it replaces it`
      } catch {
        // The editor holds invalid JSON; the save path reports that instead.
      }
    }
    return undefined
  }, [key, config.text, config.dirty])

  const pairs = useMemo(
    () => (transport === "stdio" ? parsePairs(env) : parsePairs(headers)),
    [transport, env, headers],
  )

  const definition = useMemo<UpstreamConfig>(() => {
    const entry: UpstreamConfig = {}
    if (description.trim() !== "") entry.description = description.trim()
    if (transport !== "auto") entry.type = transport

    if (transport === "stdio" || transport === "auto") {
      if (command.trim() !== "") entry.command = command.trim()
      const list = splitLines(args)
      if (list.length > 0) entry.args = list
      if (cwd.trim() !== "") entry.cwd = cwd.trim()
      if (Object.keys(pairs.values).length > 0) entry.env = pairs.values
    }
    if (transport !== "stdio") {
      if (url.trim() !== "") entry.url = url.trim()
      if (Object.keys(pairs.values).length > 0) entry.headers = pairs.values
    }

    if (startPolicy !== "") entry.startPolicy = startPolicy
    if (idleTimeout.trim() !== "") entry.idleTimeout = idleTimeout.trim()
    if (maxTools.trim() !== "") {
      const parsed = Number.parseInt(maxTools, 10)
      if (Number.isFinite(parsed) && parsed > 0) entry.maxToolsPerServer = parsed
    }
    return entry
  }, [transport, description, command, args, cwd, pairs, url, startPolicy, idleTimeout, maxTools])

  const targetProblem = useMemo(() => {
    if (transport === "stdio") {
      return command.trim() === "" ? "a command is required" : undefined
    }
    if (transport === "http" || transport === "sse") {
      if (url.trim() === "") return "a URL is required"
      if (!/^https?:\/\//i.test(url.trim())) return "the URL must start with http:// or https://"
      return undefined
    }
    // auto: the daemon decides, so at least one destination must be present.
    if (command.trim() === "" && url.trim() === "") {
      return "give either a command or a URL"
    }
    return undefined
  }, [transport, command, url])

  const problem = pairs.problem ?? keyProblem ?? targetProblem
  const canSubmit = problem === undefined && busy === null

  function reset() {
    setKey("")
    setTransport("stdio")
    setDescription("")
    setCommand("")
    setArgs("")
    setCwd("")
    setEnv("")
    setUrl("")
    setHeaders("")
    setStartPolicy("")
    setIdleTimeout("")
    setMaxTools("")
    setAdvanced(false)
    setResult(null)
    setError(null)
    setBusy(null)
  }

  function close() {
    if (busy !== null) return
    reset()
    onClose()
  }

  async function runTest() {
    setBusy("test")
    setError(null)
    try {
      setResult(await client.testServer(definition, key === "" ? "test" : key))
    } catch (err) {
      setResult(null)
      setError(err)
    } finally {
      setBusy(null)
    }
  }

  async function add() {
    setBusy("add")
    setError(null)
    try {
      await config.mutate((doc) => {
        serversOf(doc)[key] = definition
      })
      toast.success(`Added ${key}`, ["The daemon is connecting to it now."])
      bump()
      onAdded?.(key)
      reset()
      onClose()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(null)
    }
  }

  return (
    <Modal
      open={open}
      onClose={close}
      title="Add an MCP server"
      subtitle="Anything that speaks MCP over stdio or HTTP works; the tools it offers appear in the Tools tab."
      size="lg"
      footer={
        <>
          <button type="button" className="btn" onClick={close} disabled={busy !== null}>
            Cancel
          </button>
          <button type="button" className="btn" onClick={runTest} disabled={!canSubmit}>
            {busy === "test" && <Spinner />}
            Test connection
          </button>
          <button type="button" className="btn btn-primary" onClick={add} disabled={!canSubmit}>
            {busy === "add" && <Spinner />}
            Add and apply
          </button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Key" hint="The prefix its tools get: my-thing.echo">
            <input
              className="field mono"
              value={key}
              autoFocus
              spellCheck={false}
              placeholder="github"
              onChange={(event) => setKey(event.target.value.trim())}
            />
          </Field>

          <Field label="Transport">
            <select
              className="field"
              value={transport}
              onChange={(event) => setTransport(event.target.value as Transport)}
            >
              <option value="stdio">stdio — a local program</option>
              <option value="http">http — Streamable HTTP</option>
              <option value="sse">sse — deprecated HTTP+SSE</option>
              <option value="auto">auto — decide from the fields</option>
            </select>
          </Field>
        </div>

        <Field label="Description" hint="Optional. Shows up in this UI and in tools/list listings.">
          <input
            className="field"
            value={description}
            placeholder="Issues and pull requests"
            onChange={(event) => setDescription(event.target.value)}
          />
        </Field>

        {transport === "stdio" || transport === "auto" ? (
          <div className="space-y-4 rounded-lg border border-zinc-200 p-3 dark:border-zinc-800">
            <p className="label">Local program</p>

            <Field label="Command">
              <input
                className="field mono"
                value={command}
                spellCheck={false}
                placeholder="npx"
                onChange={(event) => setCommand(event.target.value)}
              />
            </Field>

            <Field label="Arguments" hint="One per line.">
              <textarea
                className="field mono h-20 resize-y"
                value={args}
                spellCheck={false}
                placeholder={"-y\n@modelcontextprotocol/server-github"}
                onChange={(event) => setArgs(event.target.value)}
              />
            </Field>

            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="Working directory">
                <input
                  className="field mono"
                  value={cwd}
                  spellCheck={false}
                  placeholder="optional"
                  onChange={(event) => setCwd(event.target.value)}
                />
              </Field>

              <Field label="Environment" hint="KEY=VALUE per line. ${env:NAME} reads the daemon's environment.">
                <textarea
                  className="field mono h-20 resize-y"
                  value={env}
                  spellCheck={false}
                  placeholder={"GITHUB_TOKEN=${env:GITHUB_TOKEN}"}
                  onChange={(event) => setEnv(event.target.value)}
                />
              </Field>
            </div>
          </div>
        ) : null}

        {transport !== "stdio" ? (
          <div className="space-y-4 rounded-lg border border-zinc-200 p-3 dark:border-zinc-800">
            <p className="label">Remote server</p>

            <Field label="URL">
              <input
                className="field mono"
                value={url}
                spellCheck={false}
                placeholder="https://mcp.example.com/mcp"
                onChange={(event) => setUrl(event.target.value)}
              />
            </Field>

            <Field label="Headers" hint={"One per line, as “Name: Value” or NAME=VALUE."}>
              <textarea
                className="field mono h-20 resize-y"
                value={headers}
                spellCheck={false}
                placeholder={"Authorization: Bearer ${env:EXAMPLE_TOKEN}"}
                onChange={(event) => setHeaders(event.target.value)}
              />
            </Field>
          </div>
        ) : null}

        <div>
          <button type="button" className="btn btn-ghost btn-sm" onClick={() => setAdvanced(!advanced)}>
            {advanced ? "Hide advanced settings" : "Advanced settings"}
          </button>

          {advanced && (
            <div className="mt-3 grid gap-4 rounded-lg border border-zinc-200 p-3 sm:grid-cols-3 dark:border-zinc-800">
              <Field label="Start policy" hint="Inherits the default when empty.">
                <select
                  className="field"
                  value={startPolicy}
                  onChange={(event) => setStartPolicy(event.target.value)}
                >
                  <option value="">default</option>
                  <option value="eager">eager — connect at startup</option>
                  <option value="lazy">lazy — connect on first use</option>
                  <option value="never">never — only when asked</option>
                </select>
              </Field>

              <Field label="Idle timeout" hint="For example 10m.">
                <input
                  className="field mono"
                  value={idleTimeout}
                  spellCheck={false}
                  placeholder="10m"
                  onChange={(event) => setIdleTimeout(event.target.value)}
                />
              </Field>

              <Field label="Max tools" hint="Cap what this server may publish.">
                <input
                  className="field mono"
                  value={maxTools}
                  inputMode="numeric"
                  placeholder="unlimited"
                  onChange={(event) => setMaxTools(event.target.value)}
                />
              </Field>
            </div>
          )}
        </div>

        {problem !== undefined && <p className="text-xs text-amber-700 dark:text-amber-400">{problem}</p>}

        {error !== null && <ErrorNote error={error} />}

        {result?.ok === true && (
          <div className="rounded-lg border border-emerald-200 bg-emerald-50 p-3 text-sm dark:border-emerald-900/60 dark:bg-emerald-950/40">
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-medium text-emerald-900 dark:text-emerald-200">Connected</span>
              <Badge tone="ok">{result.info.protocolVersion}</Badge>
              {result.info.legacy && <Badge tone="warn">legacy handshake</Badge>}
              <Badge tone="neutral">
                {result.info.tools.length} {result.info.tools.length === 1 ? "tool" : "tools"}
              </Badge>
            </div>
            <p className="mt-1 text-xs text-emerald-900/80 dark:text-emerald-200/80">
              {result.info.serverInfo?.name ?? "unnamed server"}
              {result.info.serverInfo?.version ? ` ${result.info.serverInfo.version}` : ""}
            </p>
            {result.info.tools.length > 0 && (
              <ul className="mono mt-2 max-h-32 overflow-auto text-xs text-emerald-900 dark:text-emerald-200">
                {result.info.tools.map((tool) => (
                  <li key={tool.name} className="truncate">
                    {key === "" ? "test" : key}.{tool.name}
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}

        {result?.ok === false && <ErrorNote error={new ApiError(0, result.error)} />}

        <details className="text-xs">
          <summary className="cursor-pointer text-zinc-500 select-none dark:text-zinc-400">
            Show the JSON this will write
          </summary>
          <Code className="mt-2">{JSON.stringify({ [key === "" ? "key" : key]: definition }, null, 2)}</Code>
        </details>
      </div>
    </Modal>
  )
}
