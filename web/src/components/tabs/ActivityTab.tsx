import { useCallback, useEffect, useState } from "react"
import { useApp } from "../../app-context"
import { clock, dateTime, duration, millis, plural } from "../../lib/format"
import type { AuditEvent, AuditStats } from "../../types"
import { useToast } from "../Toast"
import {
  Badge,
  Code,
  ConfirmButton,
  EmptyState,
  ErrorNote,
  Field,
  Modal,
  Section,
  Spinner,
  type Tone,
} from "../ui"

const statusTone: Record<string, Tone> = {
  ok: "ok",
  error: "error",
  denied: "warn",
}

/** ActivityTab is the audit log: one row per tool call that reached the proxy. */
export function ActivityTab() {
  const { client, revision, bump } = useApp()
  const toast = useToast()

  const [events, setEvents] = useState<AuditEvent[]>([])
  const [stats, setStats] = useState<AuditStats | null>(null)
  const [enabled, setEnabled] = useState(true)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<unknown>(null)
  const [selected, setSelected] = useState<AuditEvent | null>(null)
  const [working, setWorking] = useState(false)

  const [status, setStatus] = useState("")
  const [server, setServer] = useState("")
  const [clientFilter, setClientFilter] = useState("")
  const [limit, setLimit] = useState(100)
  const [pruneDays, setPruneDays] = useState(30)

  const load = useCallback(async () => {
    try {
      const result = await client.audit({
        status,
        server,
        client: clientFilter,
        limit,
      })
      setEvents(result.events ?? [])
      setStats(result.stats)
      setEnabled(result.enabled)
      setError(null)
    } catch (err) {
      setError(err)
    } finally {
      setLoading(false)
    }
  }, [client, status, server, clientFilter, limit])

  useEffect(() => {
    void load()
  }, [load, revision])

  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) void load()
    }, 5000)
    return () => window.clearInterval(timer)
  }, [load])

  const prune = useCallback(async () => {
    setWorking(true)
    try {
      const result = await client.pruneAudit(pruneDays)
      toast.success(
        `Removed ${result.removed} ${plural(result.removed, "event")}`,
        [`Older than ${result.days} days.`],
      )
      await load()
      bump()
    } catch (err) {
      toast.report(err, "Pruning")
    } finally {
      setWorking(false)
    }
  }, [client, pruneDays, toast, load, bump])

  if (enabled === false) {
    return (
      <Section title="Activity">
        <EmptyState title="Auditing is switched off">
          Enable it in the configuration under <span className="mono">server.audit</span> to record
          tool calls here.
        </EmptyState>
      </Section>
    )
  }

  const byStatus = stats?.byStatus ?? {}

  return (
    <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Stat label="Recorded calls" value={stats?.total ?? 0} />
        <Stat
          label="Failures"
          value={(byStatus.error ?? 0) + (byStatus.denied ?? 0)}
          tone={(byStatus.error ?? 0) > 0 ? "error" : "ok"}
        />
        <Stat label="Dropped" value={stats?.dropped ?? 0} tone={(stats?.dropped ?? 0) > 0 ? "warn" : undefined} />
        <Stat label="Newest" value={stats?.newestAt ? clock(stats.newestAt) : "—"} mono />
      </div>

      <Section
        title="Calls"
        description="Every tools/call that reached the proxy, and how it ended."
        bodyClassName="p-0"
        actions={
          <>
            <select className="field w-36" value={status} onChange={(event) => setStatus(event.target.value)}>
              <option value="">Any result</option>
              <option value="ok">ok</option>
              <option value="error">error</option>
              <option value="denied">denied</option>
            </select>
            <input
              className="field w-40"
              placeholder="Server"
              value={server}
              onChange={(event) => setServer(event.target.value)}
            />
            <input
              className="field w-40"
              placeholder="Client"
              value={clientFilter}
              onChange={(event) => setClientFilter(event.target.value)}
            />
            <select
              className="field w-28"
              value={String(limit)}
              onChange={(event) => setLimit(Number(event.target.value))}
            >
              {[50, 100, 250, 500].map((value) => (
                <option key={value} value={value}>
                  last {value}
                </option>
              ))}
            </select>
          </>
        }
      >
        {error !== null && <ErrorNote error={error} className="m-4" />}

        {loading && events.length === 0 ? (
          <div className="flex items-center justify-center gap-2 p-10 text-sm text-zinc-500">
            <Spinner /> Reading the log
          </div>
        ) : events.length === 0 ? (
          <EmptyState title="Nothing recorded yet">
            Calls made through a harness show up here within a second or two.
          </EmptyState>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full border-collapse">
              <thead>
                <tr className="border-b border-zinc-200 dark:border-zinc-800">
                  <th className="th">Time</th>
                  <th className="th">Client</th>
                  <th className="th">Tool</th>
                  <th className="th">Result</th>
                  <th className="th">Took</th>
                </tr>
              </thead>
              <tbody>
                {events.map((event) => (
                  <tr
                    key={event.id}
                    className="cursor-pointer border-b border-zinc-100 last:border-0 hover:bg-zinc-50 dark:border-zinc-800/60 dark:hover:bg-zinc-800/40"
                    onClick={() => setSelected(event)}
                  >
                    <td className="td mono whitespace-nowrap text-zinc-500 dark:text-zinc-400" title={dateTime(event.time)}>
                      {clock(event.time)}
                    </td>
                    <td className="td whitespace-nowrap">{event.client}</td>
                    <td className="td mono">
                      {event.tool}
                      {event.upstreamTool && event.upstreamTool !== event.tool && (
                        <span className="text-zinc-400 dark:text-zinc-500"> → {event.upstreamTool}</span>
                      )}
                    </td>
                    <td className="td whitespace-nowrap">
                      <Badge tone={statusTone[event.status] ?? "neutral"}>{event.status}</Badge>
                      {event.error && (
                        <span className="ml-2 text-xs text-red-700 dark:text-red-400">
                          {event.error.slice(0, 80)}
                        </span>
                      )}
                    </td>
                    <td className="td mono whitespace-nowrap text-zinc-500 dark:text-zinc-400">
                      {millis(event.durationMs)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>

      <Section
        title="Housekeeping"
        description={
          stats?.oldestAt
            ? `The oldest record is from ${dateTime(stats.oldestAt)} (${duration(
                Math.round((Date.now() - new Date(stats.oldestAt).getTime()) / 1000),
              )} ago).`
            : "Nothing has been recorded yet."
        }
        actions={
          <>
            <Field label="Keep" className="w-24">
              <input
                className="field mono"
                inputMode="numeric"
                value={String(pruneDays)}
                onChange={(event) => setPruneDays(Number(event.target.value) || 30)}
              />
            </Field>
            <span className="pt-5 text-xs text-zinc-500 dark:text-zinc-400">days</span>
            <ConfirmButton className="btn btn-sm mt-5" confirmText="Prune now" onConfirm={prune} disabled={working}>
              Prune older events
            </ConfirmButton>
          </>
        }
      >
        <p className="text-xs text-zinc-500 dark:text-zinc-400">
          Pruning deletes history; it does not touch the configuration or any running server.
        </p>
      </Section>

      <Modal
        open={selected !== null}
        onClose={() => setSelected(null)}
        title={selected?.tool ?? ""}
        subtitle={selected ? dateTime(selected.time) : undefined}
        footer={
          <button type="button" className="btn" onClick={() => setSelected(null)}>
            Close
          </button>
        }
      >
        {selected && (
          <div className="space-y-3">
            <dl className="grid grid-cols-2 gap-3 text-sm">
              <Detail label="Client" value={selected.client} />
              <Detail label="Target server" value={selected.server} />
              <Detail label="Exposed tool" value={selected.tool} mono />
              <Detail label="Upstream tool" value={selected.upstreamTool ?? "—"} mono />
              <Detail label="Result" value={selected.status} />
              <Detail label="Duration" value={millis(selected.durationMs)} />
            </dl>

            {selected.error && (
              <div>
                <p className="label mb-1.5">Error</p>
                <Code className="text-red-700 dark:text-red-300">{selected.error}</Code>
              </div>
            )}
          </div>
        )}
      </Modal>
    </div>
  )
}

function Stat({
  label,
  value,
  tone,
  mono,
}: {
  label: string
  value: number | string
  tone?: Tone
  mono?: boolean
}) {
  const colours: Record<string, string> = {
    error: "text-red-600 dark:text-red-400",
    warn: "text-amber-600 dark:text-amber-400",
    ok: "text-zinc-900 dark:text-zinc-100",
  }

  return (
    <div className="surface p-4">
      <p className="label">{label}</p>
      <p className={`mt-1 text-2xl font-semibold ${mono ? "mono text-lg" : ""} ${tone ? colours[tone] : ""}`}>
        {value}
      </p>
    </div>
  )
}

function Detail({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="min-w-0">
      <dt className="label">{label}</dt>
      <dd className={`mt-0.5 truncate ${mono ? "mono" : ""}`} title={value}>
        {value}
      </dd>
    </div>
  )
}
