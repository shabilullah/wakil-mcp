import { Suspense, lazy, useCallback, useEffect, useMemo, useState } from "react"
import { ApiError, Client } from "./api"
import { AppContext, endpoint, type AppContextValue, type TabID } from "./app-context"
import { ActivityTab } from "./components/tabs/ActivityTab"
import { ClientsTab } from "./components/tabs/ClientsTab"
import { HarnessTab } from "./components/tabs/HarnessTab"
import { ServersTab } from "./components/tabs/ServersTab"
import { ToolsTab } from "./components/tabs/ToolsTab"
import { TokenGate } from "./components/TokenGate"
import { Badge, CopyButton, ErrorNote, Spinner } from "./components/ui"
import { plural } from "./lib/format"
import { useConfig } from "./lib/useConfig"
import type { Status } from "./types"

// The raw editor brings CodeMirror with it, which is only worth downloading
// when the Config tab is actually opened.
const ConfigTab = lazy(() =>
  import("./components/tabs/ConfigTab").then((module) => ({ default: module.ConfigTab })),
)

const TOKEN_KEY = "wakil.adminToken"
const THEME_KEY = "wakil.theme"

const TABS: Array<{ id: TabID; label: string }> = [
  { id: "servers", label: "Servers" },
  { id: "tools", label: "Tools" },
  { id: "harness", label: "Harness" },
  { id: "clients", label: "Clients" },
  { id: "config", label: "Config" },
  { id: "activity", label: "Activity" },
]

/** tabFromHash reads the current tab out of the URL, so links are shareable. */
function tabFromHash(): TabID {
  const raw = window.location.hash.replace(/^#\/?/, "")
  return TABS.find((tab) => tab.id === raw)?.id ?? "servers"
}

export function App() {
  const [token, setToken] = useState(() => localStorage.getItem(TOKEN_KEY) ?? "")
  const [dark, setDark] = useState(() => localStorage.getItem(THEME_KEY) !== "light")
  const [tab, setTab] = useState<TabID>(tabFromHash)

  const [status, setStatus] = useState<Status | null>(null)
  const [statusError, setStatusError] = useState<unknown>(null)
  const [rejected, setRejected] = useState(false)
  const [firstLoadDone, setFirstLoadDone] = useState(false)
  const [busy, setBusy] = useState(false)
  const [revision, setRevision] = useState(0)

  const client = useMemo(() => new Client(token), [token])

  const bump = useCallback(() => setRevision((value) => value + 1), [])
  const config = useConfig(client, bump)

  const loadStatus = useCallback(async () => {
    setBusy(true)
    try {
      setStatus(await client.status())
      setStatusError(null)
      setRejected(false)
    } catch (error) {
      if (error instanceof ApiError && error.isAuthFailure) {
        setRejected(true)
      } else {
        setStatusError(error)
      }
    } finally {
      setBusy(false)
      setFirstLoadDone(true)
    }
  }, [client])

  // Reload the summary on mount, after any change, and on a slow timer so the
  // header stays honest while a long-running action settles.
  useEffect(() => {
    void loadStatus()
  }, [loadStatus, revision])

  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) void loadStatus()
    }, 5000)
    return () => window.clearInterval(timer)
  }, [loadStatus])

  // Fetch the configuration once per credential, so the raw editor and the
  // Servers tab both have something to work with before either is opened.
  useEffect(() => {
    void config.sync().catch((error: unknown) => setStatusError(error))
  }, [config.sync])

  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark)
    localStorage.setItem(THEME_KEY, dark ? "dark" : "light")
  }, [dark])

  useEffect(() => {
    const label = TABS.find((entry) => entry.id === tab)?.label ?? "wakil-mcp"
    document.title = `${label} · wakil-mcp`
  }, [tab])

  useEffect(() => {
    function onHashChange() {
      setTab(tabFromHash())
    }
    window.addEventListener("hashchange", onHashChange)
    return () => window.removeEventListener("hashchange", onHashChange)
  }, [])

  const go = useCallback((next: TabID) => {
    window.location.hash = `/${next}`
    setTab(next)
  }, [])

  const signOut = useCallback(() => {
    localStorage.removeItem(TOKEN_KEY)
    setToken("")
    setStatus(null)
    setStatusError(null)
    setRejected(false)
  }, [])

  const acceptToken = useCallback((next: string) => {
    localStorage.setItem(TOKEN_KEY, next)
    setToken(next)
    setRejected(false)
  }, [])

  const context = useMemo<AppContextValue>(
    () => ({
      client,
      token,
      status,
      statusError,
      revision,
      refresh: bump,
      bump,
      config,
      dark,
      setDark,
      go,
      signOut,
    }),
    [client, token, status, statusError, revision, bump, config, dark, go, signOut],
  )

  // A rejected credential has to be replaced before anything else can work.
  if (!firstLoadDone) {
    return (
      <div className="flex min-h-dvh items-center justify-center text-zinc-500">
        <Spinner className="size-6" />
      </div>
    )
  }

  if (rejected) {
    return (
      <TokenGate
        onAccept={acceptToken}
        notice={
          token === ""
            ? undefined
            : "That token was not accepted. The daemon may have been restarted with a new one."
        }
      />
    )
  }

  if (!status) {
    return (
      <div className="mx-auto flex min-h-dvh max-w-lg flex-col items-center justify-center gap-4 p-6">
        <ErrorNote error={statusError} className="w-full" />
        <button type="button" className="btn" onClick={bump}>
          Try again
        </button>
      </div>
    )
  }

  const url = endpoint(status)

  return (
    <AppContext.Provider value={context}>
      <div className="min-h-dvh">
        <header className="sticky top-0 z-30 border-b border-zinc-200 bg-white/85 backdrop-blur dark:border-zinc-800 dark:bg-zinc-950/85">
          <div className="mx-auto flex max-w-[110rem] flex-wrap items-center gap-x-3 gap-y-2 px-4 py-2.5">
            <div className="flex items-center gap-2.5">
              <svg viewBox="0 0 32 32" className="size-7 shrink-0" aria-hidden="true">
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
              <span className="text-sm font-semibold">wakil-mcp</span>
              <Badge tone="muted">{status.version}</Badge>
            </div>

            <StatusSummary status={status} />

            <div className="ml-auto flex items-center gap-2">
              {url !== "" && (
                <CopyButton
                  value={url}
                  label={url}
                  className="btn btn-sm mono hidden max-w-[20rem] items-center gap-1.5 truncate xl:inline-flex"
                />
              )}

              <button
                type="button"
                className="btn btn-sm btn-ghost"
                onClick={bump}
                aria-label="Refresh"
                title="Refresh"
              >
                <Spinner className={busy ? "size-3.5" : "hidden size-3.5"} />
                {!busy && (
                  <svg viewBox="0 0 24 24" className="size-3.5" fill="none" aria-hidden="true">
                    <path
                      d="M20 12a8 8 0 1 1-2.3-5.7M20 4v4.5h-4.5"
                      stroke="currentColor"
                      strokeWidth="2"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    />
                  </svg>
                )}
                <span className="hidden sm:inline">Refresh</span>
              </button>

              <button
                type="button"
                className="btn btn-sm btn-ghost"
                onClick={() => setDark(!dark)}
                aria-label="Switch theme"
                title={dark ? "Switch to the light theme" : "Switch to the dark theme"}
              >
                {dark ? "Light" : "Dark"}
              </button>

              {status.authEnabled && (
                <button type="button" className="btn btn-sm btn-ghost" onClick={signOut} title="Forget the admin token">
                  Sign out
                </button>
              )}
            </div>
          </div>

          <nav className="mx-auto flex max-w-[110rem] gap-1 overflow-x-auto px-3">
            {TABS.map((entry) => (
              <button
                key={entry.id}
                type="button"
                onClick={() => go(entry.id)}
                aria-current={tab === entry.id ? "page" : undefined}
                className={`-mb-px cursor-pointer border-b-2 px-3 py-2 text-sm font-medium whitespace-nowrap transition ${
                  tab === entry.id
                    ? "border-emerald-500 text-zinc-900 dark:text-zinc-100"
                    : "border-transparent text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-200"
                }`}
              >
                {entry.label}
              </button>
            ))}
          </nav>
        </header>

        <main className="mx-auto max-w-[110rem] space-y-5 px-4 py-5">
          {statusError !== null && (
            <ErrorNote error={statusError} className="print:hidden" />
          )}

          <Suspense
            fallback={
              <div className="flex items-center justify-center gap-2 p-16 text-sm text-zinc-500">
                <Spinner /> Loading
              </div>
            }
          >
            {tab === "servers" && <ServersTab />}
            {tab === "tools" && <ToolsTab />}
            {tab === "harness" && <HarnessTab />}
            {tab === "clients" && <ClientsTab />}
            {tab === "config" && <ConfigTab />}
            {tab === "activity" && <ActivityTab />}
          </Suspense>
        </main>

        <footer className="mx-auto max-w-[110rem] px-4 pt-2 pb-8 text-xs text-zinc-500 dark:text-zinc-500">
          <span className="mono">{status.dataDir}</span>
        </footer>
      </div>
    </AppContext.Provider>
  )
}

/** StatusSummary is the header's at-a-glance state of the proxy. */
function StatusSummary({ status }: { status: Status }) {
  const states = status.states ?? {}
  const failing = states.error ?? 0
  const connecting = (states.starting ?? 0) + (states.stopped ?? 0)
  const collisions = status.collisions ?? []

  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <Badge tone="neutral" title="Tools exposed to harnesses">
        {status.toolCount} {plural(status.toolCount, "tool")}
      </Badge>
      <Badge tone="neutral" title={`${status.serverCount} configured upstream servers`}>
        {status.serverCount} {plural(status.serverCount, "server")}
      </Badge>

      {failing > 0 && (
        <Badge tone="error" title="Servers whose last connection attempt failed">
          {failing} failing
        </Badge>
      )}
      {connecting > 0 && (
        <Badge tone="info" title="Servers that are not connected yet">
          {connecting} idle
        </Badge>
      )}
      {collisions.length > 0 && (
        <Badge tone="warn" title="Tool names that had to be disambiguated">
          {collisions.length} renamed
        </Badge>
      )}
      {!status.authEnabled && (
        <Badge tone="warn" title="The MCP endpoint accepts any caller">
          no auth
        </Badge>
      )}
    </div>
  )
}
