import { useCallback, useEffect, useState } from "react"
import { useApp } from "../../app-context"
import { parseConfig, renderConfig } from "../../lib/config"
import { JsonEditor } from "../JsonEditor"
import { useToast } from "../Toast"
import { Badge, CopyButton, ErrorNote, Spinner } from "../ui"

/**
 * ConfigTab is the raw editor over config.json.
 *
 * It edits the file itself rather than a projection of it, so a field this UI
 * has never heard of still survives an edit.
 */
export function ConfigTab() {
  const { client, config, dark, go } = useApp()
  const toast = useToast()

  const [problems, setProblems] = useState<string[] | null>(null)
  const [validating, setValidating] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [syncing, setSyncing] = useState(false)

  // Notice edits made elsewhere (the CLI, another browser) without ever
  // clobbering a buffer that has unsaved work in it.
  useEffect(() => {
    const timer = window.setInterval(() => {
      if (document.hidden) return
      config.sync().catch(() => {
        // A failed background sync is reported by the header's status poll.
      })
    }, 5000)
    return () => window.clearInterval(timer)
  }, [config.sync])

  const validate = useCallback(async () => {
    setValidating(true)
    setError(null)
    try {
      const result = await client.validateConfig(config.text)
      setProblems(result.problems ?? [])
      if (result.valid) toast.success("The configuration is valid")
    } catch (err) {
      setError(err)
      setProblems(null)
    } finally {
      setValidating(false)
    }
  }, [client, config.text, toast])

  const format = useCallback(() => {
    setError(null)
    try {
      config.setText(renderConfig(parseConfig(config.text)))
      setProblems([])
    } catch (err) {
      setError(err)
      setProblems(null)
    }
  }, [config])

  // A validation result only describes the text it was run against, so an edit
  // invalidates it rather than leaving a stale verdict on screen.
  const edit = useCallback(
    (next: string) => {
      config.setText(next)
      setProblems(null)
      setError(null)
    },
    [config],
  )

  const save = useCallback(async () => {
    setError(null)
    setProblems(null)
    try {
      await config.save()
      toast.success("Saved and applied")
    } catch (err) {
      setError(err)
    }
  }, [config, toast])

  const reload = useCallback(async () => {
    setError(null)
    try {
      await config.sync({ force: true })
      setProblems(null)
      toast.info("Reloaded the configuration from disk")
    } catch (err) {
      setError(err)
    }
  }, [config, toast])

  const refreshNow = useCallback(async () => {
    setSyncing(true)
    try {
      await config.sync()
    } catch (err) {
      setError(err)
    } finally {
      setSyncing(false)
    }
  }, [config])

  return (
    <div className="space-y-4">
      <section className="surface overflow-hidden">
        <header className="card-head">
          <div className="min-w-0">
            <h2 className="text-sm font-semibold">
              Configuration
              {config.dirty && (
                <span className="ml-2">
                  <Badge tone="warn">unsaved changes</Badge>
                </span>
              )}
            </h2>
            <p className="mono mt-0.5 truncate text-xs text-zinc-500 dark:text-zinc-400">
              {config.path || "loading…"}
            </p>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            {config.path && <CopyButton value={config.path} label="Copy the path" className="btn btn-sm" />}
            <button
              type="button"
              className="btn btn-sm"
              onClick={refreshNow}
              disabled={syncing}
              title="Re-read the file from the daemon"
            >
              {syncing && <Spinner className="size-3.5" />}
              Reload
            </button>
            <button type="button" className="btn btn-sm" onClick={format} disabled={!config.loaded}>
              Format
            </button>
            <button type="button" className="btn btn-sm" onClick={validate} disabled={validating || !config.loaded}>
              {validating && <Spinner className="size-3.5" />}
              Validate
            </button>
            <button
              type="button"
              className="btn btn-primary btn-sm"
              onClick={save}
              disabled={config.saving || !config.dirty}
            >
              {config.saving && <Spinner className="size-3.5" />}
              Save and apply
            </button>
          </div>
        </header>

        {config.stale && (
          <div className="flex flex-wrap items-center gap-3 border-b border-amber-200 bg-amber-50 px-4 py-2 text-xs text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200">
            <span>
              The file changed on disk while these edits were unsaved. Loading it discards what is in
              the editor; saving keeps it and overwrites the file.
            </span>
            <button type="button" className="btn btn-sm" onClick={reload}>
              Load the file
            </button>
          </div>
        )}

        {error !== null && <ErrorNote error={error} className="m-4" />}

        {problems !== null && problems.length > 0 && (
          <div className="m-4 rounded-lg border border-red-200 bg-red-50 p-3 dark:border-red-900/60 dark:bg-red-950/40">
            <p className="text-sm font-medium text-red-800 dark:text-red-300">
              {problems.length} {problems.length === 1 ? "problem" : "problems"}
            </p>
            <ul className="mt-2 list-disc space-y-1 pl-5 text-xs text-red-800 dark:text-red-300">
              {problems.map((problem, index) => (
                <li key={index} className="break-words">
                  {problem}
                </li>
              ))}
            </ul>
          </div>
        )}

        {problems !== null && problems.length === 0 && (
          <p className="border-b border-emerald-200 bg-emerald-50 px-4 py-2 text-xs text-emerald-900 dark:border-emerald-900/60 dark:bg-emerald-950/40 dark:text-emerald-200">
            The configuration is valid and would apply cleanly.
          </p>
        )}

        <div className="h-[65vh] min-h-[24rem]">
          {config.loaded ? (
            <JsonEditor
              value={config.text}
              onChange={edit}
              onSave={save}
              dark={dark}
              className="h-full"
            />
          ) : (
            <div className="flex h-full items-center justify-center gap-2 text-sm text-zinc-500">
              <Spinner /> Loading the configuration
            </div>
          )}
        </div>

        <footer className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-zinc-200 px-4 py-2 text-xs text-zinc-500 dark:border-zinc-800 dark:text-zinc-400">
          <span>
            <span className="kbd">⌘S</span> / <span className="kbd">Ctrl+S</span> saves and applies
          </span>
          <span>Unknown fields are kept as written.</span>
          <span>
            Secrets use <span className="mono">${"{env:NAME}"}</span> or{" "}
            <span className="mono">${"{file:path}"}</span> and are resolved when a server starts.
          </span>
        </footer>
      </section>

      <section className="surface p-4">
        <h2 className="text-sm font-semibold">Saving applies immediately</h2>
        <p className="mt-1 text-xs leading-relaxed text-zinc-600 dark:text-zinc-300">
          A saved configuration replaces the running one without a restart: servers that were removed
          are dropped along with their tools, added ones connect, and the namespacing rules are
          re-applied. Harnesses see the change as a tool list update, and a client that misses it can
          simply call <span className="mono">tools/list</span> again.
        </p>
        <p className="mt-2 text-xs text-zinc-500 dark:text-zinc-400">
          A change that parses but cannot be applied, such as a bind address already in use, is
          reported here while the file on disk is left holding the new text.{" "}
          <button type="button" className="link cursor-pointer" onClick={() => go("servers")}>
            See the servers
          </button>
          .
        </p>
      </section>
    </div>
  )
}
