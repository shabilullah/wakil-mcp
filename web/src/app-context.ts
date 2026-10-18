import { createContext, useContext } from "react"
import type { Client } from "./api"
import type { ConfigController } from "./lib/useConfig"
import type { Status } from "./types"

export type TabID = "servers" | "tools" | "harness" | "clients" | "config" | "activity"

export interface AppContextValue {
  /** client is bound to the current admin token. */
  client: Client
  token: string
  /** status is the last successful poll. */
  status: Status | null
  /** statusError is the failure from the most recent poll, if any. */
  statusError: unknown
  /** revision changes whenever data another tab may be showing has changed. */
  revision: number
  /** refresh re-reads the status and tells every tab to reload. */
  refresh: () => void
  /** bump tells the other tabs that data changed, without a status poll. */
  bump: () => void
  /** config is the shared configuration document and its editor buffer. */
  config: ConfigController
  dark: boolean
  setDark: (dark: boolean) => void
  go: (tab: TabID) => void
  signOut: () => void
}

export const AppContext = createContext<AppContextValue | null>(null)

/** useApp reaches the shared state from a tab. */
export function useApp(): AppContextValue {
  const value = useContext(AppContext)
  if (!value) throw new Error("useApp was called outside the provider")
  return value
}

/** endpoint is the URL a harness connects to. */
export function endpoint(status: Status | null): string {
  if (!status) return ""
  return `${status.publicUrl.replace(/\/+$/, "")}${status.mcpEndpoint}`
}
