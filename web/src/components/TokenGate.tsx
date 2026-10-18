import { useState, type FormEvent } from "react"
import { Client } from "../api"
import { ErrorNote, Spinner } from "./ui"

/**
 * TokenGate asks for the admin token and refuses to hand control back until the
 * daemon has accepted it.
 *
 * The gate is only reached when a request actually failed with 401 or 403, so a
 * daemon running with authentication disabled never shows it.
 */
export function TokenGate({
  onAccept,
  notice,
}: {
  onAccept: (token: string) => void
  notice?: string
}) {
  const [value, setValue] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)

  async function submit(event: FormEvent) {
    event.preventDefault()
    const candidate = value.trim()
    if (candidate === "" || busy) return

    setBusy(true)
    setError(null)
    try {
      // The token is only stored once the daemon has accepted it, so a typo
      // does not leave a broken credential behind.
      await new Client(candidate).status()
      onAccept(candidate)
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-dvh items-center justify-center p-4">
      <form onSubmit={submit} className="surface w-full max-w-md p-6">
        <div className="mb-6 flex items-center gap-3">
          <svg viewBox="0 0 32 32" className="size-9 shrink-0" aria-hidden="true">
            <rect width="32" height="32" rx="7" className="fill-zinc-900 dark:fill-zinc-800" />
            <path
              d="M8 12.5 12 21l4-8.5 4 8.5 4-8.5"
              className="stroke-emerald-400"
              strokeWidth="2.5"
              fill="none"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
          <div>
            <h1 className="text-base font-semibold">wakil-mcp</h1>
            <p className="text-xs text-zinc-500 dark:text-zinc-400">one MCP endpoint for every harness</p>
          </div>
        </div>

        {notice && (
          <p className="mb-4 rounded-lg border border-amber-200 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200">
            {notice}
          </p>
        )}

        <label className="block space-y-1.5">
          <span className="label">Admin token</span>
          <input
            className="field mono"
            name="adminToken"
            type="password"
            autoComplete="off"
            autoFocus
            spellCheck={false}
            placeholder="wakil_…"
            value={value}
            onChange={(event) => setValue(event.target.value)}
          />
        </label>

        {error !== null && <ErrorNote error={error} className="mt-4" />}

        <button type="submit" className="btn btn-primary mt-5 w-full" disabled={busy || value.trim() === ""}>
          {busy && <Spinner />}
          Unlock
        </button>

        <p className="mt-4 text-xs leading-relaxed text-zinc-500 dark:text-zinc-400">
          The admin token is separate from the tokens harnesses use. It is shown once, when the
          configuration is first created. If it is lost, mint a new one with{" "}
          <code className="kbd">wakil-mcp token admin</code>.
        </p>
      </form>
    </div>
  )
}
