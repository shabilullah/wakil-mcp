import { ApiError, type Client } from "../api"
import type { UpstreamConfig } from "../types"

/** ConfigDoc is the configuration file as an editable JSON document. */
export interface ConfigDoc {
  version?: number
  server?: Record<string, unknown>
  defaults?: Record<string, unknown>
  clients?: unknown[]
  mcpServers?: Record<string, UpstreamConfig>
  [key: string]: unknown
}

/** parseConfig decodes config text, rejecting anything that is not an object. */
export function parseConfig(text: string): ConfigDoc {
  let value: unknown
  try {
    value = JSON.parse(text)
  } catch (error) {
    throw new ApiError(0, `the configuration is not valid JSON: ${(error as Error).message}`)
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new ApiError(0, "the configuration must be a JSON object")
  }
  return value as ConfigDoc
}

/**
 * serversOf returns the mcpServers map, creating it when the file has none.
 *
 * Callers mutate the returned object and re-render the document; nothing here
 * touches the file.
 */
export function serversOf(doc: ConfigDoc): Record<string, UpstreamConfig> {
  const existing = doc.mcpServers
  if (existing === undefined || existing === null || typeof existing !== "object") {
    const created: Record<string, UpstreamConfig> = {}
    doc.mcpServers = created
    return created
  }
  return existing
}

/** renderConfig formats a document the way the daemon writes it. */
export function renderConfig(doc: ConfigDoc): string {
  return `${JSON.stringify(doc, null, 2)}\n`
}

/** serverOf reads one server definition, or undefined. */
export function serverOf(doc: ConfigDoc, key: string): UpstreamConfig | undefined {
  return doc.mcpServers?.[key]
}

/** cloneConfig deep-copies a document, so a failed edit cannot corrupt the buffer. */
export function cloneDoc(doc: ConfigDoc): ConfigDoc {
  return JSON.parse(JSON.stringify(doc)) as ConfigDoc
}

/** saveConfigTo writes a document through the API, keeping the client in one place. */
export async function saveConfigTo(client: Client, doc: ConfigDoc): Promise<void> {
  await client.saveConfig(renderConfig(doc))
}
