import { useCallback, useEffect, useState } from "react"
import { useApp } from "../../app-context"
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
  type ServerInventory,
} from "../../lib/rules"
import type { AllowRule, ClientView } from "../../types"
import { RulePicker } from "../RulePicker"
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
  TokenReveal,
  Toggle,
} from "../ui"

/** describeRule renders a rule for the table. */
function describeRule(rule: AllowRule | undefined, emptyText: string): string {
  if (!rule) return emptyText

  const servers = rule.servers ?? []
  const tools = rule.tools ?? {}
  const toolEntries = Object.entries(tools)

  const parts: string[] = []
  parts.push(servers.length === 0 ? emptyText : servers.join(", "))
  for (const [server, patterns] of toolEntries) {
    parts.push(`${server}: ${patterns.join(", ")}`)
  }
  return parts.join(" · ")
}

/** ClientsTab manages the credentials harnesses use. */
export function ClientsTab() {
  const { client, revision, bump } = useApp()
  const toast = useToast()

  const [clients, setClients] = useState<ClientView[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<unknown>(null)
  const [working, setWorking] = useState<string>("")

  const [createOpen, setCreateOpen] = useState(false)
  const [revealed, setRevealed] = useState<{ name: string; token: string; note?: string } | null>(null)

  const load = useCallback(async () => {
    try {
      setClients(await client.clients())
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

  async function act(name: string, verb: string, run: () => Promise<unknown>) {
    setWorking(name)
    try {
      await run()
      await load()
      bump()
    } catch (err) {
      toast.report(err, `${verb} ${name}`)
    } finally {
      setWorking("")
    }
  }

  return (
    <div className="space-y-4">
      <Section
        title="Harness clients"
        description="Each token maps to one client. Tokens can be limited to particular servers and tools, and revoked without touching the rest."
        bodyClassName="p-0"
        actions={
          <button type="button" className="btn btn-primary btn-sm" onClick={() => setCreateOpen(true)}>
            New client
          </button>
        }
      >
        {error !== null && <ErrorNote error={error} className="m-4" />}

        {loading && clients.length === 0 ? (
          <div className="flex items-center justify-center gap-2 p-10 text-sm text-zinc-500">
            <Spinner /> Loading clients
          </div>
        ) : clients.length === 0 ? (
          <EmptyState
            title="No client tokens"
            action={
              <button type="button" className="btn btn-primary btn-sm" onClick={() => setCreateOpen(true)}>
                Create one
              </button>
            }
          >
            With authentication on, a harness needs a token to reach the MCP endpoint.
          </EmptyState>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full border-collapse">
              <thead>
                <tr className="border-b border-zinc-200 dark:border-zinc-800">
                  <th className="th">Client</th>
                  <th className="th">Token</th>
                  <th className="th">Can reach</th>
                  <th className="th">Allowed</th>
                  <th className="th">Denied</th>
                  <th className="th">Actions</th>
                </tr>
              </thead>
              <tbody>
                {clients.map((entry) => (
                  <tr key={entry.name} className="border-b border-zinc-100 last:border-0 dark:border-zinc-800/60">
                    <td className="td whitespace-nowrap">
                      <span className="font-medium">{entry.name}</span>
                      {entry.disabled && (
                        <Badge tone="warn" title="The token is refused while the client is disabled">
                          disabled
                        </Badge>
                      )}
                    </td>
                    <td className="td">
                      {entry.hasToken ? (
                        <Badge tone="ok">set</Badge>
                      ) : (
                        <Badge tone="muted" title="Without a hash this profile cannot authenticate">
                          none
                        </Badge>
                      )}
                    </td>
                    <td className="td whitespace-nowrap">
                      {entry.visibleTools} {entry.visibleTools === 1 ? "tool" : "tools"}
                    </td>
                    <td className="td max-w-xs text-xs text-zinc-500 dark:text-zinc-400">
                      {describeRule(entry.allow, "all servers")}
                    </td>
                    <td className="td max-w-xs text-xs text-zinc-500 dark:text-zinc-400">
                      {entry.deny?.servers?.length || Object.keys(entry.deny?.tools ?? {}).length
                        ? describeRule(entry.deny, "nothing")
                        : "nothing"}
                    </td>
                    <td className="td">
                      <div className="flex flex-wrap items-center gap-2">
                        <Toggle
                          checked={!entry.disabled}
                          disabled={working === entry.name}
                          label={`Enable ${entry.name}`}
                          onChange={(enabled) =>
                            act(entry.name, enabled ? "Enabling" : "Disabling", () =>
                              client.setClientEnabled(entry.name, enabled),
                            )
                          }
                        />
                        <button
                          type="button"
                          className="btn btn-sm"
                          disabled={working === entry.name}
                          onClick={() =>
                            act(entry.name, "Rotating", async () => {
                              const response = await client.rotateClient(entry.name)
                              setRevealed({ name: entry.name, token: response.token, note: response.note })
                            })
                          }
                          title="Issue a new token and stop the old one from working"
                        >
                          Rotate
                        </button>
                        <ConfirmButton
                          confirmText="Delete it"
                          disabled={working === entry.name}
                          onConfirm={() => act(entry.name, "Deleting", () => client.deleteClient(entry.name))}
                        >
                          Delete
                        </ConfirmButton>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>

      <CreateClientDialog
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={(name, token, note) => {
          setRevealed({ name, token, note })
          void load()
          bump()
        }}
      />

      <Modal
        open={revealed !== null}
        onClose={() => setRevealed(null)}
        title={`Token for ${revealed?.name ?? ""}`}
        subtitle="This is the only time it is shown."
        dismissable={false}
        footer={
          <button type="button" className="btn btn-primary" onClick={() => setRevealed(null)}>
            I have stored it
          </button>
        }
      >
        {revealed && <TokenReveal token={revealed.token} note={revealed.note} />}
      </Modal>
    </div>
  )
}

function CreateClientDialog({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: (name: string, token: string, note?: string) => void
}) {
  const { client } = useApp()

  const [name, setName] = useState("")
  const [allow, setAllow] = useState<RuleState>(emptyRule())
  const [deny, setDeny] = useState<RuleState>(emptyRule())
  const [allowExtraServers, setAllowExtraServers] = useState("")
  const [allowExtraTools, setAllowExtraTools] = useState("")
  const [denyExtraServers, setDenyExtraServers] = useState("")
  const [denyExtraTools, setDenyExtraTools] = useState("")

  const [inventory, setInventory] = useState<ServerInventory[]>([])
  const [inventoryLoading, setInventoryLoading] = useState(false)
  const [inventoryError, setInventoryError] = useState<unknown>(null)

  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)

  // The pickers need the live list of servers and their tools, which is why it
  // is fetched when the dialog opens rather than when the tab is built.
  useEffect(() => {
    if (!open) return
    let cancelled = false

    setInventoryLoading(true)
    void Promise.all([client.servers(), client.tools()])
      .then(([servers, tools]) => {
        if (cancelled) return
        setInventory(buildInventory(servers, tools.tools))
        setInventoryError(null)
      })
      .catch((err) => {
        if (!cancelled) setInventoryError(err)
      })
      .finally(() => {
        if (!cancelled) setInventoryLoading(false)
      })

    return () => {
      cancelled = true
    }
  }, [open, client])

  function reset() {
    setName("")
    setAllow(emptyRule())
    setDeny(emptyRule())
    setAllowExtraServers("")
    setAllowExtraTools("")
    setDenyExtraServers("")
    setDenyExtraTools("")
    setInventoryError(null)
    setError(null)
    setBusy(false)
  }

  function close() {
    if (busy) return
    reset()
    onClose()
  }

  const allowParsed = parseToolLines(allowExtraTools)
  const denyParsed = parseToolLines(denyExtraTools)
  const parseError = allowParsed.error ?? denyParsed.error

  const allowRule = mergeExtras("allow", encodeRule(allow, "allow"), {
    servers: parseServers(allowExtraServers),
    tools: allowParsed.tools,
  })
  const denyRule = mergeExtras("deny", encodeRule(deny, "deny"), {
    servers: parseServers(denyExtraServers),
    tools: denyParsed.tools,
  })

  // Both lists are checked for names that match nothing, because that is the
  // mistake this dialog exists to prevent.
  const keys = inventory.map((server) => server.key)
  const unknownPatterns = [
    ...new Set([
      ...patternsThatMatchNothing(allowRule.servers ?? [], keys),
      ...patternsThatMatchNothing(Object.keys(allowRule.tools ?? {}), keys),
      ...patternsThatMatchNothing(denyRule.servers ?? [], keys),
      ...patternsThatMatchNothing(Object.keys(denyRule.tools ?? {}), keys),
    ]),
  ]

  async function submit() {
    if (parseError !== undefined) return

    setBusy(true)
    setError(null)
    try {
      const response = await client.createClient(name.trim(), allowRule, denyRule)

      onCreated(response.client.name, response.token, response.note)
      reset()
      onClose()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={close}
      size="lg"
      title="New client"
      subtitle="Tick the servers and tools this harness may reach. Nothing ticked means no restriction."
      footer={
        <>
          <button type="button" className="btn" onClick={close} disabled={busy}>
            Cancel
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={submit}
            disabled={busy || name.trim() === "" || parseError !== undefined}
          >
            {busy && <Spinner />}
            Create and show token
          </button>
        </>
      }
    >
      <div className="space-y-4">
        <Field label="Name" hint="Shows up in the audit log.">
          <input
            className="field"
            value={name}
            autoFocus
            placeholder="laptop"
            onChange={(event) => setName(event.target.value)}
          />
        </Field>

        <RulePicker
          mode="allow"
          state={allow}
          onChange={setAllow}
          inventory={inventory}
          loading={inventoryLoading}
          error={inventoryError}
        />

        <RulePicker
          mode="deny"
          state={deny}
          onChange={setDeny}
          inventory={inventory}
          loading={inventoryLoading}
          error={inventoryError}
        />

        <div className="space-y-1 text-xs text-zinc-500 dark:text-zinc-400">
          <p>
            Allowed: <span className="text-zinc-700 dark:text-zinc-300">{describeState(allow, "allow")}</span>.
            Ticking a server on its own allows every tool it publishes, including tools it gains later. Ticking
            individual tools instead makes this an explicit allowlist: a server with no tick stops being
            reachable.
          </p>
          <p>
            Denied: <span className="text-zinc-700 dark:text-zinc-300">{describeState(deny, "deny")}</span>. A
            deny always wins over an allow, so a server or tool listed there is unreachable even when it is also
            ticked above.
          </p>
          <p>The proxy's own <span className="mono">_wakil.*</span> tools are visible to every client.</p>
        </div>

        {unknownPatterns.length > 0 && (
          <p className="rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-800 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-300">
            No configured server matches{" "}
            {unknownPatterns.map((pattern) => (
              <span key={pattern} className="mono">
                {pattern}{" "}
              </span>
            ))}
            . The token will be created, but anything only those patterns cover stays unreachable.
          </p>
        )}

        {parseError !== undefined && <ErrorNote error={new Error(parseError)} />}
        {error !== null && <ErrorNote error={error} />}

        <details className="rounded-lg border border-zinc-200 px-3 py-2 text-xs dark:border-zinc-800">
          <summary className="cursor-pointer text-zinc-500 select-none dark:text-zinc-400">
            Advanced: extra glob patterns
          </summary>
          <div className="mt-3 space-y-3">
            <p className="text-zinc-500 dark:text-zinc-400">
              These are added on top of the ticks. A pattern may use <span className="mono">*</span> for any run
              of characters and <span className="mono">?</span> for one, and is matched case-insensitively
              against both the exposed name and the server's own name.
            </p>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Allow servers" hint="Comma separated patterns.">
                <input
                  className="field mono"
                  value={allowExtraServers}
                  placeholder="github, lin*"
                  onChange={(event) => setAllowExtraServers(event.target.value)}
                />
              </Field>
              <Field label="Allow tools" hint="One server:pattern per line.">
                <textarea
                  className="field mono h-20 resize-y"
                  value={allowExtraTools}
                  placeholder={"github:get_*\nlinear:list_*"}
                  onChange={(event) => setAllowExtraTools(event.target.value)}
                />
              </Field>
              <Field label="Deny servers" hint="Comma separated patterns.">
                <input
                  className="field mono"
                  value={denyExtraServers}
                  placeholder="experiments"
                  onChange={(event) => setDenyExtraServers(event.target.value)}
                />
              </Field>
              <Field label="Deny tools" hint="One server:pattern per line.">
                <textarea
                  className="field mono h-20 resize-y"
                  value={denyExtraTools}
                  placeholder={"github:delete_*"}
                  onChange={(event) => setDenyExtraTools(event.target.value)}
                />
              </Field>
            </div>
          </div>
        </details>

        <details className="text-xs">
          <summary className="cursor-pointer text-zinc-500 select-none dark:text-zinc-400">
            Show the rule this will send
          </summary>
          <Code className="mt-2">{JSON.stringify({ allow: allowRule, deny: denyRule }, null, 2)}</Code>
        </details>
      </div>
    </Modal>
  )
}
