import { useCallback, useEffect, useState } from "react"
import { endpoint, useApp } from "../../app-context"
import type { Snippet } from "../../types"
import { Badge, Code, CopyButton, CopyField, EmptyState, ErrorNote, Section, Spinner } from "../ui"

/** HarnessTab hands out the copy-and-paste configuration for each client. */
export function HarnessTab() {
  const { client, status, go } = useApp()

  const [names, setNames] = useState<string[]>([])
  const [active, setActive] = useState("")
  const [snippets, setSnippets] = useState<Snippet[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<unknown>(null)

  const url = endpoint(status)

  useEffect(() => {
    client
      .harnesses()
      .then((list) => {
        setNames(list)
        setActive((current) => (current === "" ? (list[0] ?? "") : current))
      })
      .catch((err: unknown) => setError(err))
  }, [client])

  const load = useCallback(
    async (name: string) => {
      if (name === "") return
      setLoading(true)
      try {
        const response = await client.harness(name)
        setSnippets(response.snippets ?? [])
        setError(null)
      } catch (err) {
        setError(err)
        setSnippets([])
      } finally {
        setLoading(false)
      }
    },
    [client],
  )

  useEffect(() => {
    void load(active)
  }, [load, active])

  return (
    <div className="space-y-4">
      <Section
        title="Connect a harness"
        description="Point a client at this endpoint once, and every server you have added shows up in it."
      >
        <div className="space-y-3">
          <CopyField value={url} />
          <div className="flex flex-wrap gap-2 text-xs text-zinc-500 dark:text-zinc-400">
            <Badge tone={status?.authEnabled ? "ok" : "warn"}>
              {status?.authEnabled ? "a token is required" : "authentication is off"}
            </Badge>
            <span>
              Harness tokens are separate from the admin token and can be limited to a subset of
              servers and tools.{" "}
              <button type="button" className="link cursor-pointer" onClick={() => go("clients")}>
                Manage clients
              </button>
              .
            </span>
          </div>
        </div>
      </Section>

      {error !== null && <ErrorNote error={error} />}

      <div className="grid gap-4 lg:grid-cols-[15rem_1fr]">
        <Section title="Harness" bodyClassName="p-2">
          <ul className="space-y-0.5">
            {names.map((name) => (
              <li key={name}>
                <button
                  type="button"
                  onClick={() => setActive(name)}
                  className={`w-full cursor-pointer rounded-lg px-3 py-2 text-left text-sm font-medium transition ${
                    active === name
                      ? "bg-emerald-600 text-white"
                      : "hover:bg-zinc-100 dark:hover:bg-zinc-800"
                  }`}
                >
                  {name}
                </button>
              </li>
            ))}
          </ul>
        </Section>

        <div className="space-y-4">
          {loading ? (
            <Section>
              <div className="flex items-center justify-center gap-2 p-8 text-sm text-zinc-500">
                <Spinner /> Rendering snippets
              </div>
            </Section>
          ) : snippets.length === 0 ? (
            <Section>
              <EmptyState title="Nothing to show for that harness" />
            </Section>
          ) : (
            snippets.map((snippet) => (
              <Section
                key={`${snippet.label}-${snippet.filename ?? ""}`}
                title={snippet.label}
                description={snippet.filename ? <span className="mono">{snippet.filename}</span> : undefined}
                bodyClassName="p-0"
                actions={
                  <>
                    <Badge tone="muted">{snippet.language}</Badge>
                    {snippet.deprecated && (
                      <Badge tone="warn" title="Kept for older clients; prefer the HTTP transport">
                        deprecated
                      </Badge>
                    )}
                    <CopyButton value={snippet.content} label="Copy" />
                  </>
                }
              >
                {snippet.note && (
                  <p className="border-b border-zinc-200 px-4 py-2 text-xs text-zinc-500 dark:border-zinc-800 dark:text-zinc-400">
                    {snippet.note}
                  </p>
                )}
                <Code className="m-0 rounded-none border-0">{snippet.content}</Code>
              </Section>
            ))
          )}

          <Section title="A word about tokens">
            <ul className="list-disc space-y-1.5 pl-5 text-xs text-zinc-600 dark:text-zinc-300">
              <li>
                A token is shown once, when it is created. The snippets above use a placeholder, so
                replace it with a real token{" "}
                <button type="button" className="link cursor-pointer" onClick={() => go("clients")}>
                  from the Clients tab
                </button>
                .
              </li>
              <li>
                Prefer the environment-variable form where the client supports it: the token then
                stays out of the config file and out of source control.
              </li>
              <li>
                A client that can only launch a child process uses the <code className="kbd">stdio</code>{" "}
                bridge form. It relays frames to this same daemon, so nothing is re-declared.
              </li>
              <li>
                Tools are namespaced <code className="kbd">server.tool</code>, so two servers can both
                offer an <code className="kbd">echo</code>.
              </li>
            </ul>
          </Section>
        </div>
      </div>

      {status && status.toolCount === 0 && (
        <Section title="Nothing is published yet">
          <p className="text-sm text-zinc-600 dark:text-zinc-300">
            A harness connecting now would see only this proxy's own helper tools.{" "}
            <button type="button" className="link cursor-pointer" onClick={() => go("servers")}>
              Add a server
            </button>{" "}
            first.
          </p>
        </Section>
      )}
    </div>
  )
}
