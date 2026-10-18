import { useState } from "react"
import { pickedTools, type RuleState, type ServerInventory } from "../lib/rules"
import { Badge, ErrorNote, Spinner, type Tone } from "./ui"

const stateTone: Record<string, Tone> = {
  ready: "ok",
  starting: "info",
  stopped: "muted",
  disabled: "muted",
  error: "error",
}

/** Tick is a three-state checkbox: on, off, or on-for-some. */
export function Tick({
  state,
  label,
  disabled,
  onToggle,
}: {
  state: "on" | "off" | "mixed"
  label: string
  disabled?: boolean
  onToggle: (next: boolean) => void
}) {
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={state === "mixed" ? "mixed" : state === "on"}
      aria-label={label}
      disabled={disabled}
      onClick={() => onToggle(state !== "on")}
      className={`grid size-4 shrink-0 cursor-pointer place-items-center rounded border text-[0.625rem] leading-none transition disabled:cursor-not-allowed disabled:opacity-40 ${
        state === "off"
          ? "border-zinc-300 bg-white text-transparent hover:border-emerald-500 dark:border-zinc-600 dark:bg-zinc-900 dark:hover:border-emerald-500"
          : "border-emerald-600 bg-emerald-600 text-white"
      }`}
    >
      {state === "mixed" ? "–" : "✓"}
    </button>
  )
}

/**
 * RulePicker is the checkbox view of one allow or deny rule: every upstream,
 * its tools, and select-all shortcuts.
 *
 * The state it renders is the same rule the dialog submits, so there is no
 * second source of truth to drift from what the daemon stores.
 */
export function RulePicker({
  mode,
  state,
  onChange,
  inventory,
  loading,
  error,
}: {
  mode: "allow" | "deny"
  state: RuleState
  /** onChange takes an updater rather than a value, so a handler that flips
   * several boxes in one go cannot lose all but the last of them. */
  onChange: (update: (current: RuleState) => RuleState) => void
  inventory: ServerInventory[]
  loading: boolean
  error: unknown
}) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})

  const allow = mode === "allow"

  function setWhole(key: string, on: boolean) {
    onChange((current) => {
      const whole = on ? [...new Set([...current.whole, key])] : current.whole.filter((k) => k !== key)
      const picks = { ...current.picks }
      delete picks[key]
      return { whole, picks }
    })
  }

  function setAll(on: boolean) {
    onChange(() =>
      on ? { whole: inventory.map((server) => server.key), picks: {} } : { whole: [], picks: {} },
    )
  }

  function toggleTool(key: string, tool: string, on: boolean) {
    onChange((current) => {
      // Unticking a tool under an unticked server still has to start from the
      // full list when the server was granted in full, or the first click would
      // drop every other tool.
      const known = inventory.find((server) => server.key === key)?.tools ?? []
      const from = current.whole.includes(key) ? known : pickedTools(current, key)
      const next = on ? [...new Set([...from, tool])] : from.filter((name) => name !== tool)

      const picks = { ...current.picks }
      if (next.length === 0) delete picks[key]
      else picks[key] = next

      return { whole: current.whole.filter((k) => k !== key), picks }
    })
  }

  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-zinc-200 px-3 py-2 dark:border-zinc-800">
        <div className="min-w-0">
          <span className="text-sm font-medium">{allow ? "Allow" : "Deny"}</span>
          <span className="ml-2 text-xs text-zinc-500 dark:text-zinc-400">
            {allow ? "servers and tools this client may use" : "servers and tools to block"}
          </span>
        </div>
        <div className="flex items-center gap-1">
          <button type="button" className="btn btn-sm" onClick={() => setAll(true)} disabled={inventory.length === 0}>
            All
          </button>
          <button type="button" className="btn btn-sm" onClick={() => setAll(false)}>
            None
          </button>
        </div>
      </div>

      <div className="max-h-60 overflow-y-auto px-3 py-2">
        {error !== null && <ErrorNote error={error} className="mb-2" />}

        {loading && inventory.length === 0 ? (
          <div className="flex items-center justify-center gap-2 py-6 text-sm text-zinc-500">
            <Spinner /> Loading servers
          </div>
        ) : inventory.length === 0 ? (
          <p className="py-4 text-center text-sm text-zinc-500 dark:text-zinc-400">
            No upstream servers are configured yet, so there is nothing to pick.
          </p>
        ) : (
          <ul className="space-y-1">
            {inventory.map((server) => {
              const inWhole = state.whole.includes(server.key)
              const picked = pickedTools(state, server.key)
              // Coverage of the tools that are known, rather than a count, so a
              // leftover pick for a renamed tool cannot make a server look
              // fully selected.
              const full = inWhole || (server.tools.length > 0 && server.tools.every((tool) => picked.includes(tool)))
              const boxState = full ? "on" : picked.length > 0 ? "mixed" : "off"
              const open = expanded[server.key] === true
              const tone = server.state ? stateTone[server.state] : undefined

              return (
                <li key={server.key} className="rounded-md px-1 py-0.5 hover:bg-zinc-50 dark:hover:bg-zinc-800/50">
                  <div className="flex items-center gap-2">
                    <Tick
                      state={boxState}
                      label={`${allow ? "Allow" : "Deny"} ${server.key}`}
                      onToggle={(next) => setWhole(server.key, next)}
                    />
                    <button
                      type="button"
                      className="mono min-w-0 truncate text-left hover:underline"
                      onClick={() => setExpanded((current) => ({ ...current, [server.key]: !open }))}
                    >
                      {server.key}
                    </button>

                    <span className="shrink-0 text-xs text-zinc-500 dark:text-zinc-400">
                      {server.known
                        ? `${server.tools.length} ${server.tools.length === 1 ? "tool" : "tools"}`
                        : "no tools yet"}
                    </span>

                    {server.state && tone && (
                      <Badge tone={tone} title={server.lastError ?? `The server is ${server.state}.`}>
                        {server.state}
                      </Badge>
                    )}

                    {server.tools.length > 0 && (
                      <button
                        type="button"
                        className="btn btn-ghost btn-sm ml-auto shrink-0"
                        aria-expanded={open}
                        onClick={() => setExpanded((current) => ({ ...current, [server.key]: !open }))}
                      >
                        {open ? "Hide tools" : "Tools"}
                      </button>
                    )}
                  </div>

                  {open && server.tools.length > 0 && (
                    <div className="mt-1 mb-1 ml-6">
                      <div className="flex flex-wrap gap-x-3 gap-y-1">
                        {server.tools.map((tool) => {
                          const ticked = inWhole || picked.includes(tool)
                          return (
                            <label
                              key={tool}
                              className="flex cursor-pointer items-center gap-1.5 text-xs"
                              title={server.namesAreUpstream ? "The server's own tool name." : undefined}
                            >
                              <Tick
                                state={ticked ? "on" : "off"}
                                label={`${allow ? "Allow" : "Deny"} ${server.key} ${tool}`}
                                onToggle={(next) => toggleTool(server.key, tool, next)}
                              />
                              <span className="mono">{tool}</span>
                            </label>
                          )
                        })}
                      </div>
                      <div className="mt-1.5 flex items-center gap-2">
                        <button
                          type="button"
                          className="link text-xs"
                          onClick={() => setWhole(server.key, true)}
                        >
                          All {server.tools.length}
                        </button>
                        <button type="button" className="link text-xs" onClick={() => setWhole(server.key, false)}>
                          None
                        </button>
                        {server.namesAreUpstream && (
                          <span className="text-xs text-zinc-500 dark:text-zinc-400">
                            not contributing right now, so these are its own names
                          </span>
                        )}
                      </div>
                    </div>
                  )}
                </li>
              )
            })}
          </ul>
        )}
      </div>
    </div>
  )
}
