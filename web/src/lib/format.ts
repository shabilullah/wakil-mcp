/** Formatting and small browser helpers shared by the tabs. */

/** plural picks the singular or plural word for a count. */
export function plural(count: number, singular: string, pluralForm?: string): string {
  if (count === 1) return singular
  return pluralForm ?? `${singular}s`
}

/** relative renders a timestamp as "3s ago" or "in 5s". */
export function relative(iso: string | undefined): string {
  if (!iso) return ""
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ""
  // The daemon's zero time is not a real timestamp.
  if (then <= 0) return ""

  const delta = Date.now() - then
  const future = delta < 0
  const seconds = Math.round(Math.abs(delta) / 1000)

  const text = duration(seconds)
  return future ? `in ${text}` : `${text} ago`
}

/** duration renders a number of seconds compactly. */
export function duration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h`
  return `${Math.floor(hours / 24)}d`
}

/** millis renders a whole-millisecond duration, keeping sub-millisecond honest. */
export function millis(value: number): string {
  if (value <= 0) return "<1 ms"
  return `${value} ms`
}

/** dateTime renders an absolute local timestamp. */export function dateTime(iso: string | undefined): string {
  if (!iso) return ""
  const at = new Date(iso)
  if (Number.isNaN(at.getTime()) || at.getTime() <= 0) return ""
  return at.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  })
}

/** clock renders just the time of day. */
export function clock(iso: string | undefined): string {
  if (!iso) return ""
  const at = new Date(iso)
  if (Number.isNaN(at.getTime()) || at.getTime() <= 0) return ""
  return at.toLocaleTimeString(undefined, { hour12: false })
}

/** pretty renders any JSON value as indented text. */
export function pretty(value: unknown): string {
  if (value === undefined || value === null) return ""
  if (typeof value === "string") return value
  try {
    return JSON.stringify(value, null, 2)
  } catch {
    return String(value)
  }
}

/** truncate shortens a string, keeping the end of a long identifier visible. */
export function truncate(value: string, max = 90): string {
  if (value.length <= max) return value
  return `${value.slice(0, max - 1)}…`
}

/** masks a token, so a screenshot does not leak a credential. */
export function maskToken(token: string): string {
  if (token.length <= 14) return token
  return `${token.slice(0, 10)}…${token.slice(-6)}`
}

/**
 * copy writes text to the clipboard and reports whether it worked.
 *
 * navigator.clipboard only exists in a secure context, and this daemon is
 * usually reached over plain HTTP on a LAN address, so the old
 * execCommand path is a real fallback rather than a curiosity.
 */
export async function copy(text: string): Promise<boolean> {
  if (typeof navigator !== "undefined" && navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // Fall through to the legacy path.
    }
  }

  const area = document.createElement("textarea")
  area.value = text
  area.setAttribute("readonly", "")
  area.style.position = "fixed"
  area.style.top = "-1000px"
  area.style.opacity = "0"
  document.body.appendChild(area)

  const selection = document.getSelection()
  const saved = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null

  area.select()
  let ok = false
  try {
    ok = document.execCommand("copy")
  } catch {
    ok = false
  }
  document.body.removeChild(area)

  if (selection && saved) {
    selection.removeAllRanges()
    selection.addRange(saved)
  }
  return ok
}
