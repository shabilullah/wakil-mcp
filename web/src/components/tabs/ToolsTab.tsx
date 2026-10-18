import { useCallback, useEffect, useMemo, useState } from "react"
import { useApp } from "../../app-context"
import { pretty } from "../../lib/format"
import type { Collision, ToolEntry } from "../../types"
import { Badge, Code, CopyButton, EmptyState, ErrorNote, Modal, Section, Spinner } from "../ui"

/** ToolsTab is the verification view: exactly what a harness will see. */
export function ToolsTab() {
  const { client, revision } = useApp()

  const [tools, setTools] = useState<ToolEntry[]>([])
  const [servers, setServers] = useState<string[]>([])
  const [collisions, setCollisions] = useState<Collision[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<unknown>(null)
  const [query, setQuery] = useState("")
  const [serverFilter, setServerFilter] = useState("")
  const [selected, setSelected] = useState<ToolEntry | null>(null)

  const load = useCallback(async () => {
    try {
      const result = await client.tools()
      setTools(result.tools)
      setServers(result.servers)
      setCollisions(result.collisions)
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

  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) void load()
    }, 10000)
    return () => window.clearInterval(timer)
  }, [load])

  const shown = useMemo(() => {
    const needle = query.trim().toLowerCase()

    return tools.filter((tool) => {
      if (serverFilter !== "" && tool.serverKey !== serverFilter) return false
      if (needle === "") return true

      return (
        tool.exposed.toLowerCase().includes(needle) ||
        tool.upstreamName.toLowerCase().includes(needle) ||
        tool.serverKey.toLowerCase().includes(needle) ||
        (tool.tool.description ?? "").toLowerCase().includes(needle) ||
        (tool.tool.title ?? "").toLowerCase().includes(needle)
      )
    })
  }, [tools, query, serverFilter])

  return (
    <div className="space-y-4">
      {collisions.length > 0 && (
        <Section
          title="Names that had to change"
          description="Two servers offered the same exposed name, so one of them was given a numbered suffix. Calls still reach the right server."
        >
          <ul className="space-y-2 text-sm">
            {collisions.map((collision) => (
              <li key={`${collision.serverKey}-${collision.given}`} className="flex flex-wrap items-center gap-2">
                <Badge tone="warn">{collision.wanted}</Badge>
                <span className="text-zinc-500 dark:text-zinc-400">was taken by</span>
                <span className="mono">{collision.heldBy}</span>
                <span className="text-zinc-500 dark:text-zinc-400">so</span>
                <span className="mono">{collision.serverKey}</span>
                <span className="text-zinc-500 dark:text-zinc-400">now answers to</span>
                <span className="mono font-medium">{collision.given}</span>
              </li>
            ))}
          </ul>
        </Section>
      )}

      <Section
        title="Exposed tools"
        description={
          <>
            These names are what a harness calls. Search filters on the exposed name, the upstream name
            and the description. A harness also sees three tools the proxy provides itself —{" "}
            <span className="mono">_wakil.search_tools</span>,{" "}
            <span className="mono">_wakil.describe_tool</span> and{" "}
            <span className="mono">_wakil.list_servers</span> — which are not listed here because no
            upstream server owns them.
          </>
        }
        bodyClassName="p-0"
        actions={
          <>
            <input
              className="field w-56"
              type="search"
              placeholder="Filter tools…"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
            />
            <select
              className="field w-44"
              value={serverFilter}
              onChange={(event) => setServerFilter(event.target.value)}
            >
              <option value="">All servers</option>
              {servers.map((server) => (
                <option key={server} value={server}>
                  {server}
                </option>
              ))}
            </select>
            <span className="text-xs text-zinc-500 dark:text-zinc-400">
              {shown.length} of {tools.length}
            </span>
          </>
        }
      >
        {error !== null && <ErrorNote error={error} className="m-4" />}

        {loading && tools.length === 0 ? (
          <div className="flex items-center justify-center gap-2 p-10 text-sm text-zinc-500">
            <Spinner /> Listing tools
          </div>
        ) : tools.length === 0 ? (
          <EmptyState title="No tools are exposed yet">
            Add a server in the Servers tab. As soon as it connects, its tools appear here with the
            names a harness will use.
          </EmptyState>
        ) : shown.length === 0 ? (
          <EmptyState title="Nothing matches that filter" />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full border-collapse">
              <thead>
                <tr className="border-b border-zinc-200 dark:border-zinc-800">
                  <th className="th">Exposed name</th>
                  <th className="th">Server</th>
                  <th className="th">Upstream name</th>
                  <th className="th">Description</th>
                </tr>
              </thead>
              <tbody>
                {shown.map((tool) => (
                  <tr
                    key={tool.exposed}
                    className="cursor-pointer border-b border-zinc-100 last:border-0 hover:bg-zinc-50 dark:border-zinc-800/60 dark:hover:bg-zinc-800/40"
                    onClick={() => setSelected(tool)}
                  >
                    <td className="td mono font-medium whitespace-nowrap">
                      {tool.exposed}
                      {tool.truncated && (
                        <Badge tone="warn" title="The upstream name was too long and was shortened">
                          shortened
                        </Badge>
                      )}
                      {tool.disambiguated && (
                        <Badge tone="warn" title="Renamed to avoid a collision">
                          renamed
                        </Badge>
                      )}
                      {tool.renamed && (
                        <Badge tone="info" title="Renamed by a toolAliases entry">
                          alias
                        </Badge>
                      )}
                    </td>
                    <td className="td whitespace-nowrap text-zinc-500 dark:text-zinc-400">
                      {tool.serverKey}
                    </td>
                    <td className="td mono whitespace-nowrap text-zinc-500 dark:text-zinc-400">
                      {tool.upstreamName}
                    </td>
                    <td className="td max-w-xl text-zinc-600 dark:text-zinc-300">
                      {tool.tool.description || <span className="text-zinc-400 dark:text-zinc-600">—</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>

      <Modal
        open={selected !== null}
        onClose={() => setSelected(null)}
        size="xl"
        title={selected?.exposed ?? ""}
        subtitle={
          selected ? (
            <span className="mono">
              {selected.serverKey} / {selected.upstreamName}
            </span>
          ) : undefined
        }
        footer={
          <>
            <CopyButton value={selected?.exposed ?? ""} label="Copy the name" />
            <button type="button" className="btn" onClick={() => setSelected(null)}>
              Close
            </button>
          </>
        }
      >
        {selected && (
          <div className="space-y-4">
            {(selected.tool.title || selected.tool.description) && (
              <div className="space-y-1">
                {selected.tool.title && <p className="text-sm font-medium">{selected.tool.title}</p>}
                {selected.tool.description && (
                  <p className="text-sm text-zinc-600 dark:text-zinc-300">{selected.tool.description}</p>
                )}
              </div>
            )}

            {selected.tool.annotations && (
              <div className="flex flex-wrap gap-1.5">
                {selected.tool.annotations.readOnlyHint && <Badge tone="ok">read only</Badge>}
                {selected.tool.annotations.destructiveHint && <Badge tone="error">destructive</Badge>}
                {selected.tool.annotations.idempotentHint && <Badge tone="info">idempotent</Badge>}
                {selected.tool.annotations.openWorldHint && <Badge tone="neutral">open world</Badge>}
              </div>
            )}

            <div>
              <p className="label mb-1.5">Input schema</p>
              <Code className="max-h-[40vh]">
                {pretty(selected.tool.inputSchema) || "{}"}
              </Code>
            </div>

            {selected.tool.outputSchema !== undefined && (
              <div>
                <p className="label mb-1.5">Output schema</p>
                <Code className="max-h-[30vh]">{pretty(selected.tool.outputSchema)}</Code>
              </div>
            )}
          </div>
        )}
      </Modal>
    </div>
  )
}
