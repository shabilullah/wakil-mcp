import { useCallback, useEffect, useState } from "react"
import { endpoint, useApp } from "../../app-context"
import { ApiError } from "../../api"
import { serversOf } from "../../lib/config"
import { dateTime, relative } from "../../lib/format"
import type { ServerSnapshot, ToolEntry } from "../../types"
import { AddServerDialog } from "../AddServerDialog"
import { useToast } from "../Toast"
import {
  Badge,
  Code,
  ConfirmButton,
  CopyField,
  EmptyState,
  ErrorNote,
  KeyValue,
  Modal,
  Section,
  Spinner,
  Toggle,
  type Tone,
} from "../ui"

const stateTone: Record<string, Tone> = {
  ready: "ok",
  starting: "info",
  stopped: "muted",
  disabled: "muted",
  error: "error",
}

const stateHint: Record<string, string> = {
  ready: "Connected and publishing tools",
  starting: "A connection attempt is in flight",
  stopped: "Enabled, but nothing is running",
  disabled: "Switched off in the configuration",
  error: "The last connection attempt failed",
}

/** ServersTab lists the upstreams and lets each one be driven by hand. */
export function ServersTab() {
  const { client, config, revision, bump, status } = useApp()
  const toast = useToast()

  const [servers, setServers] = useState<ServerSnapshot[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<unknown>(null)
  const [addOpen, setAddOpen] = useState(false)
  const [logs, setLogs] = useState<{ key: string; text: string } | null>(null)
  const [expanded, setExpanded] = useState<Record<string, ToolEntry[] | "loading">>({})
  const [working, setWorking] = useState<Record<string, string>>({})

  const load = useCallback(async () => {
    try {
      setServers(await client.servers())
      setError(null)
    } catch (err) {
      setError(err)
    } finally {
      setLoading(false)
    }
  }, [client])

  useEffect(() => {
    void load()
  }, [load, revision])

  // Connection state moves on its own as lazy servers start and idle ones are
  // reaped, so this tab refreshes itself.
  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) void load()
    }, 5000)
    return () => window.clearInterval(timer)
  }, [load])

  const act = useCallback(
    async (key: string, verb: string, run: () => Promise<unknown>) => {
      setWorking((current) => ({ ...current, [key]: verb }))
      try {
        await run()
        await load()
        bump()
      } catch (err) {
        toast.report(err, `${verb} ${key}`)
      } finally {
        setWorking((current) => {
          const next = { ...current }
          delete next[key]
          return next
        })
      }
    },
    [load, bump, toast],
  )

  const setEnabled = useCallback(
    (key: string, enabled: boolean) => {
      void act(key, enabled ? "Enabling" : "Disabling", () =>
        config.mutate((doc) => {
          const entry = serversOf(doc)[key]
          if (!entry) throw new ApiError(0, `${key} is no longer in the configuration`)
          entry.enabled = enabled
        }),
      )
    },
    [act, config],
  )

  const remove = useCallback(
    (key: string) => {
      void act(key, "Removing", () =>
        config.mutate((doc) => {
          delete serversOf(doc)[key]
        }),
      )
    },
    [act, config],
  )

  const showLogs = useCallback(
    async (key: string) => {
      try {
        setLogs({ key, text: "" })
        setLogs({ key, text: await client.serverStderr(key) })
      } catch (err) {
        setLogs(null)
        toast.report(err, `Reading the log of ${key}`)
      }
    },
    [client, toast],
  )

  const toggleTools = useCallback(
    async (key: string) => {
      if (expanded[key] !== undefined) {
        setExpanded((current) => {
          const next = { ...current }
          delete next[key]
          return next
        })
        return
      }

      setExpanded((current) => ({ ...current, [key]: "loading" }))
      try {
        const result = await client.tools({ server: key })
        setExpanded((current) => ({ ...current, [key]: result.tools }))
      } catch (err) {
        setExpanded((current) => {
          const next = { ...current }
          delete next[key]
          return next
        })
        toast.report(err, `Listing the tools of ${key}`)
      }
    },
    [client, expanded, toast],
  )

  return (
    <div className="space-y-4">
      <Section
        title="Upstream servers"
        description="Every enabled server publishes its tools through this endpoint. Adding one takes effect immediately."
        actions={
          <>
            <button type="button" className="btn btn-primary btn-sm" onClick={() => setAddOpen(true)}>
              Add server
            </button>
            <CopyField value={endpoint(status)} className="hidden lg:flex" />
          </>
        }
        bodyClassName="p-0"
      >
        {error !== null && <ErrorNote error={error} className="m-4" />}

        {loading && servers.length === 0 ? (
          <div className="flex items-center justify-center gap-2 p-10 text-sm text-zinc-500">
            <Spinner /> Reading the server list
          </div>
        ) : servers.length === 0 ? (
          <EmptyState
            title="No servers yet"
            action={
              <button type="button" className="btn btn-primary btn-sm" onClick={() => setAddOpen(true)}>
                Add the first server
              </button>
            }
          >
            Point this at anything that speaks MCP: a local <code className="kbd">npx</code> server, a
            binary, or a remote HTTP endpoint. Already have a snippet in a harness config? Paste it
            into the Config tab instead and it will be picked up.
          </EmptyState>
        ) : (
          <ul className="divide-y divide-zinc-200 dark:divide-zinc-800">
            {servers.map((server) => (
              <ServerRow
                key={server.key}
                server={server}
                working={working[server.key]}
                tools={expanded[server.key]}
                onToggleEnabled={(enabled) => setEnabled(server.key, enabled)}
                onAct={(verb, run) => act(server.key, verb, run)}
                onRemove={() => remove(server.key)}
                onLogs={() => showLogs(server.key)}
                onToggleTools={() => toggleTools(server.key)}
              />
            ))}
          </ul>
        )}
      </Section>

      {config.dirty && (
        <p className="text-xs text-amber-700 dark:text-amber-400">
          The configuration editor holds unsaved changes. Saving or discarding them in the Config tab
          comes first if this tab reports a conflict.
        </p>
      )}

      <AddServerDialog open={addOpen} onClose={() => setAddOpen(false)} />

      <Modal
        open={logs !== null}
        onClose={() => setLogs(null)}
        title={`Log from ${logs?.key ?? ""}`}
        subtitle="Everything the process wrote to stderr, newest last."
        size="xl"
      >
        {logs !== null && logs.text.trim() === "" ? (
          <p className="text-sm text-zinc-500 dark:text-zinc-400">
            Nothing has been written to stderr. A remote server has no process to log from.
          </p>
        ) : (
          <Code className="max-h-[55vh] whitespace-pre-wrap">{logs?.text}</Code>
        )}
      </Modal>
    </div>
  )
}

function ServerRow({
  server,
  working,
  tools,
  onToggleEnabled,
  onAct,
  onRemove,
  onLogs,
  onToggleTools,
}: {
  server: ServerSnapshot
  working?: string
  tools?: ToolEntry[] | "loading"
  onToggleEnabled: (enabled: boolean) => void
  onAct: (verb: string, run: () => Promise<unknown>) => void
  onRemove: () => void
  onLogs: () => void
  onToggleTools: () => void
}) {
  const { client } = useApp()
  const config = server.config

  const target =
    config?.url ??
    [config?.command, ...(config?.args ?? [])].filter(Boolean).join(" ") ??
    server.endpoint ??
    ""

  return (
    <li className="p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1.5">
          <div className="flex flex-wrap items-center gap-2">
            <Toggle
              checked={server.enabled}
              onChange={onToggleEnabled}
              label={`Enable ${server.key}`}
              disabled={working !== undefined}
            />
            <h3 className="mono text-sm font-semibold">{server.key}</h3>
            <Badge tone={stateTone[server.state] ?? "neutral"} title={stateHint[server.state]}>
              {server.state}
            </Badge>
            {server.transport && <Badge tone="muted">{server.transport}</Badge>}
            {server.legacy && (
              <Badge tone="warn" title={`Negotiated the legacy handshake at ${server.protocolVersion}`}>
                legacy {server.protocolVersion}
              </Badge>
            )}
            <Badge tone="neutral">
              {server.toolCount} {server.toolCount === 1 ? "tool" : "tools"}
            </Badge>
            {server.inFlight > 0 && <Badge tone="info">{server.inFlight} in flight</Badge>}
          </div>

          {config?.description && (
            <p className="text-sm text-zinc-600 dark:text-zinc-300">{config.description}</p>
          )}

          <p className="mono truncate text-xs text-zinc-500 dark:text-zinc-400" title={target}>
            {target === "" ? "no target recorded" : target}
          </p>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          {working !== undefined && (
            <span className="flex items-center gap-1.5 text-xs text-zinc-500">
              <Spinner className="size-3.5" />
              {working}
            </span>
          )}

          <button type="button" className="btn btn-sm" onClick={onToggleTools} disabled={working !== undefined}>
            {tools === undefined ? "Tools" : "Hide tools"}
          </button>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => onAct("Starting", () => client.startServer(server.key))}
            disabled={working !== undefined || !server.enabled}
            title="Connect now, without waiting for the start policy"
          >
            Start
          </button>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => onAct("Stopping", () => client.stopServer(server.key))}
            disabled={working !== undefined || server.state === "stopped" || server.state === "disabled"}
          >
            Stop
          </button>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => onAct("Restarting", () => client.restartServer(server.key))}
            disabled={working !== undefined || !server.enabled}
          >
            Restart
          </button>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => onAct("Testing", () => client.server(server.key))}
            disabled={working !== undefined}
            title="Re-read this server's state from the daemon"
          >
            Recheck
          </button>
          <button type="button" className="btn btn-sm" onClick={onLogs}>
            Log
          </button>
          <ConfirmButton onConfirm={onRemove} confirmText="Remove it" disabled={working !== undefined}>
            Remove
          </ConfirmButton>
        </div>
      </div>

      <div className="mt-3 grid gap-4 lg:grid-cols-2">
        <KeyValue
          items={[
            ["Start policy", server.startPolicy],
            ["Idle timeout", server.idleTimeout],
            ["Server", [server.serverName, server.serverVersion].filter(Boolean).join(" ")],
            ["Protocol", server.protocolVersion],
            ["Connected", relative(server.connectedAt)],
            ["Connected at", dateTime(server.connectedAt)],
            ["Last used", relative(server.lastUsedAt)],
            ["Process", server.pid ? `pid ${server.pid}` : undefined],
            ["Failures", server.failureCount > 0 ? String(server.failureCount) : undefined],
            ["Next attempt", server.nextAttemptAt ? relative(server.nextAttemptAt) : undefined],
          ]}
        />

        {server.lastError ? (
          <div className="self-start rounded-lg border border-red-200 bg-red-50 p-2.5 text-xs text-red-800 dark:border-red-900/60 dark:bg-red-950/40 dark:text-red-300">
            <p className="font-medium">Last error</p>
            <p className="mt-1 break-words">{server.lastError}</p>
          </div>
        ) : (
          server.instructions && (
            <p className="text-xs leading-relaxed text-zinc-500 dark:text-zinc-400">
              {server.instructions}
            </p>
          )
        )}
      </div>

      {tools !== undefined && (
        <div className="mt-3 rounded-lg border border-zinc-200 p-3 dark:border-zinc-800">
          {tools === "loading" ? (
            <span className="flex items-center gap-2 text-xs text-zinc-500">
              <Spinner className="size-3.5" /> Listing tools
            </span>
          ) : tools.length === 0 ? (
            <p className="text-xs text-zinc-500 dark:text-zinc-400">
              This server publishes no tools. It may be disabled, filtered to nothing, or still
              connecting.
            </p>
          ) : (
            <ul className="mono space-y-1 text-xs">
              {tools.map((tool) => (
                <li key={tool.exposed} className="flex flex-wrap items-baseline gap-2">
                  <span className="font-medium">{tool.exposed}</span>
                  <span className="text-zinc-400 dark:text-zinc-500">←</span>
                  <span className="text-zinc-500 dark:text-zinc-400">{tool.upstreamName}</span>
                  {tool.truncated && <Badge tone="warn">shortened</Badge>}
                  {tool.renamed && <Badge tone="info">aliased</Badge>}
                  {tool.disambiguated && <Badge tone="warn">renamed to avoid a clash</Badge>}
                  {tool.tool.description && (
                    <span className="truncate text-zinc-500 dark:text-zinc-400" title={tool.tool.description}>
                      {tool.tool.description}
                    </span>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </li>
  )
}
