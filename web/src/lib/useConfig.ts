import { useCallback, useMemo, useState } from "react"
import { ApiError, type Client } from "../api"
import { parseConfig, renderConfig, type ConfigDoc } from "./config"

/** ConfigState is the raw config text plus what the daemon last reported. */
interface ConfigState {
  /** text is the buffer the editor shows. */
  text: string
  /** base is the daemon's copy as of the last sync. */
  base: string
  /** loaded reports whether a sync has ever succeeded. */
  loaded: boolean
  /** path is the config file's location on the daemon's host. */
  path: string
  /** stale reports that the daemon's copy moved on while the buffer was dirty. */
  stale: boolean
}

const empty: ConfigState = { text: "", base: "", loaded: false, path: "", stale: false }

export interface ConfigController {
  text: string
  path: string
  dirty: boolean
  stale: boolean
  loaded: boolean
  saving: boolean
  setText: (text: string) => void
  /** sync re-reads the daemon's copy, adopting it when the buffer is clean. */
  sync: (options?: { force?: boolean }) => Promise<void>
  /** save writes the buffer, or the given text, and applies it live. */
  save: (text?: string) => Promise<void>
  /** mutate applies a change to the buffer and saves it. */
  mutate: (change: (doc: ConfigDoc) => void) => Promise<void>
}

/**
 * useConfig owns the configuration document for the whole app.
 *
 * The buffer outlives any one tab, so a half-finished edit is not lost by
 * clicking away. Everything that needs to change the configuration goes through
 * mutate, which is what keeps the Servers tab and the raw editor from writing
 * over each other.
 */
export function useConfig(client: Client, onChanged: () => void): ConfigController {
  const [state, setState] = useState<ConfigState>(empty)
  const [saving, setSaving] = useState(false)

  const sync = useCallback(
    async (options: { force?: boolean } = {}) => {
      const response = await client.config()
      setState((current) => {
        const clean = current.text === current.base
        if (clean || options.force) {
          return {
            text: response.json,
            base: response.json,
            loaded: true,
            path: response.path,
            stale: false,
          }
        }
        return {
          ...current,
          base: response.json,
          loaded: true,
          path: response.path,
          // Sticky on purpose: a buffer that no longer sits on top of the
          // file stays flagged until the operator resolves it by loading the
          // file or saving over it. Comparing only against the base this poll
          // just updated would clear the warning one interval after it
          // appeared, which is worse than not showing it at all.
          stale: current.stale || response.json !== current.base,
        }
      })
    },
    [client],
  )

  const save = useCallback(
    async (text?: string) => {
      const payload = text ?? state.text
      setSaving(true)
      try {
        await client.saveConfig(payload)
        setState((current) => ({ ...current, text: payload, base: payload, stale: false }))
        onChanged()
      } finally {
        setSaving(false)
      }
    },
    [client, state.text, onChanged],
  )

  const mutate = useCallback(
    async (change: (doc: ConfigDoc) => void) => {
      const fresh = await client.config()

      const clean = state.text === state.base
      if (!clean && fresh.json !== state.base) {
        setState((current) => ({ ...current, base: fresh.json, stale: true }))
        throw new ApiError(
          409,
          "the configuration changed on disk while these edits were unsaved; open the Config tab to reconcile them",
        )
      }

      const source = clean ? fresh.json : state.text
      const doc = parseConfig(source)
      change(doc)

      const next = renderConfig(doc)
      setSaving(true)
      try {
        await client.saveConfig(next)
        setState((current) => ({
          ...current,
          text: next,
          base: next,
          loaded: true,
          path: fresh.path,
          stale: false,
        }))
        onChanged()
      } finally {
        setSaving(false)
      }
    },
    [client, state.text, state.base, onChanged],
  )

  const setText = useCallback((text: string) => {
    setState((current) => ({ ...current, text }))
  }, [])

  return useMemo<ConfigController>(
    () => ({
      text: state.text,
      path: state.path,
      dirty: state.text !== state.base,
      stale: state.stale,
      loaded: state.loaded,
      saving,
      setText,
      sync,
      save,
      mutate,
    }),
    [state, saving, setText, sync, save, mutate],
  )
}
