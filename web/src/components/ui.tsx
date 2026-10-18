import { useEffect, useRef, useState, type ReactNode } from "react"
import { copy, maskToken } from "../lib/format"

/* -------------------------------------------------------------------------- */
/* Badges                                                                      */
/* -------------------------------------------------------------------------- */

export type Tone = "neutral" | "ok" | "warn" | "error" | "info" | "muted"

const toneClasses: Record<Tone, string> = {
  neutral: "bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300",
  ok: "bg-emerald-100 text-emerald-800 dark:bg-emerald-950/70 dark:text-emerald-300",
  warn: "bg-amber-100 text-amber-800 dark:bg-amber-950/70 dark:text-amber-300",
  error: "bg-red-100 text-red-800 dark:bg-red-950/70 dark:text-red-300",
  info: "bg-sky-100 text-sky-800 dark:bg-sky-950/70 dark:text-sky-300",
  muted: "bg-zinc-100 text-zinc-500 dark:bg-zinc-800/60 dark:text-zinc-500",
}

export function Badge({
  tone = "neutral",
  children,
  title,
}: {
  tone?: Tone
  children: ReactNode
  title?: string
}) {
  return (
    <span className={`chip ${toneClasses[tone]}`} title={title}>
      {children}
    </span>
  )
}

/* -------------------------------------------------------------------------- */
/* Loading                                                                     */
/* -------------------------------------------------------------------------- */

export function Spinner({ className = "size-4" }: { className?: string }) {
  return (
    <svg className={`animate-spin ${className}`} viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeOpacity="0.2" strokeWidth="3" />
      <path d="M21 12a9 9 0 0 0-9-9" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
    </svg>
  )
}

/* -------------------------------------------------------------------------- */
/* Clipboard                                                                   */
/* -------------------------------------------------------------------------- */

export function CopyButton({
  value,
  label = "Copy",
  className = "btn btn-sm",
}: {
  value: string
  label?: string
  className?: string
}) {
  const [state, setState] = useState<"idle" | "done" | "failed">("idle")
  const timer = useRef<number | undefined>(undefined)

  useEffect(() => () => window.clearTimeout(timer.current), [])

  async function run() {
    const ok = await copy(value)
    setState(ok ? "done" : "failed")
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => setState("idle"), 1600)
  }

  return (
    <button type="button" className={className} onClick={run}>
      {state === "done" ? "Copied" : state === "failed" ? "Press Ctrl+C" : label}
    </button>
  )
}

/** CopyField shows a value that is meant to be copied, in a read-only input. */
export function CopyField({ value, className = "" }: { value: string; className?: string }) {
  return (
    <div className={`flex items-stretch gap-2 ${className}`}>
      <input
        className="field mono min-w-0 flex-1"
        name="copyValue"
        value={value}
        readOnly
        onFocus={(event) => event.target.select()}
      />
      <CopyButton value={value} />
    </div>
  )
}

/* -------------------------------------------------------------------------- */
/* Buttons                                                                     */
/* -------------------------------------------------------------------------- */

/**
 * ConfirmButton asks for a second click before running a destructive action.
 * It reverts on its own, so a stray click cannot leave the UI armed.
 */
export function ConfirmButton({
  children,
  confirmText = "Confirm",
  onConfirm,
  className = "btn btn-sm btn-danger",
  disabled,
}: {
  children: ReactNode
  confirmText?: string
  onConfirm: () => void
  className?: string
  disabled?: boolean
}) {
  const [armed, setArmed] = useState(false)
  const timer = useRef<number | undefined>(undefined)

  useEffect(() => () => window.clearTimeout(timer.current), [])

  if (!armed) {
    return (
      <button
        type="button"
        className={className}
        disabled={disabled}
        onClick={() => {
          setArmed(true)
          window.clearTimeout(timer.current)
          timer.current = window.setTimeout(() => setArmed(false), 4000)
        }}
      >
        {children}
      </button>
    )
  }

  return (
    <button
      type="button"
      className="btn btn-sm border-red-500 bg-red-600 text-white hover:bg-red-500 dark:border-red-500 dark:bg-red-600 dark:text-white dark:hover:bg-red-500"
      onClick={() => {
        setArmed(false)
        window.clearTimeout(timer.current)
        onConfirm()
      }}
    >
      {confirmText}
    </button>
  )
}

/* -------------------------------------------------------------------------- */
/* Layout                                                                      */
/* -------------------------------------------------------------------------- */

export function Section({
  title,
  description,
  actions,
  children,
  bodyClassName = "p-4",
}: {
  title?: ReactNode
  description?: ReactNode
  actions?: ReactNode
  children: ReactNode
  bodyClassName?: string
}) {
  return (
    <section className="surface overflow-hidden">
      {(title || actions) && (
        <header className="card-head">
          <div className="min-w-0">
            {title && <h2 className="text-sm font-semibold">{title}</h2>}
            {description && <p className="mt-0.5 text-xs text-zinc-500 dark:text-zinc-400">{description}</p>}
          </div>
          {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
        </header>
      )}
      <div className={bodyClassName}>{children}</div>
    </section>
  )
}

export function EmptyState({
  title,
  children,
  action,
}: {
  title: string
  children?: ReactNode
  action?: ReactNode
}) {
  return (
    <div className="flex flex-col items-center gap-3 px-6 py-12 text-center">
      <p className="text-sm font-medium">{title}</p>
      {children && <div className="max-w-prose text-sm text-zinc-500 dark:text-zinc-400">{children}</div>}
      {action}
    </div>
  )
}

export function Field({
  label,
  hint,
  children,
  className = "",
}: {
  label: string
  hint?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <label className={`block space-y-1.5 ${className}`}>
      <span className="label">{label}</span>
      {children}
      {hint && <span className="block text-xs text-zinc-500 dark:text-zinc-400">{hint}</span>}
    </label>
  )
}

export function Toggle({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean
  onChange: (next: boolean) => void
  label: string
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full transition disabled:opacity-45 ${
        checked ? "bg-emerald-600" : "bg-zinc-300 dark:bg-zinc-700"
      }`}
    >
      <span
        className={`inline-block size-3.5 rounded-full bg-white shadow transition ${
          checked ? "translate-x-[1.15rem]" : "translate-x-[0.15rem]"
        }`}
      />
    </button>
  )
}

export function KeyValue({ items }: { items: Array<[string, ReactNode]> }) {
  const shown = items.filter(([, value]) => value !== undefined && value !== null && value !== "")
  if (shown.length === 0) return null

  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-xs">
      {shown.map(([key, value]) => (
        <div key={key} className="col-span-2 grid grid-cols-subgrid">
          <dt className="text-zinc-500 dark:text-zinc-400">{key}</dt>
          <dd className="min-w-0 break-words">{value}</dd>
        </div>
      ))}
    </dl>
  )
}

export function Code({ children, className = "" }: { children: ReactNode; className?: string }) {
  return (
    <pre
      className={`overflow-auto rounded-lg border border-zinc-200 bg-zinc-50 p-3 font-mono text-xs leading-relaxed dark:border-zinc-800 dark:bg-zinc-950 ${className}`}
    >
      {children}
    </pre>
  )
}

/* -------------------------------------------------------------------------- */
/* Modal                                                                       */
/* -------------------------------------------------------------------------- */

export function Modal({
  open,
  title,
  subtitle,
  onClose,
  children,
  footer,
  size = "md",
  dismissable = true,
}: {
  open: boolean
  title: ReactNode
  subtitle?: ReactNode
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
  size?: "md" | "lg" | "xl"
  dismissable?: boolean
}) {
  useEffect(() => {
    if (!open) return
    function onKey(event: KeyboardEvent) {
      if (event.key === "Escape" && dismissable) onClose()
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [open, onClose, dismissable])

  if (!open) return null

  const widths = { md: "max-w-lg", lg: "max-w-2xl", xl: "max-w-4xl" }

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-zinc-950/50 p-4 backdrop-blur-sm sm:p-8"
      onClick={() => {
        if (dismissable) onClose()
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        className={`surface my-auto w-full ${widths[size]}`}
        onClick={(event) => event.stopPropagation()}
      >
        <header className="card-head">
          <div className="min-w-0">
            <h2 className="text-sm font-semibold">{title}</h2>
            {subtitle && <p className="mt-0.5 text-xs text-zinc-500 dark:text-zinc-400">{subtitle}</p>}
          </div>
          {dismissable && (
            <button type="button" className="btn btn-ghost btn-sm" onClick={onClose} aria-label="Close">
              Close
            </button>
          )}
        </header>

        <div className="max-h-[70vh] overflow-y-auto p-4">{children}</div>

        {footer && <footer className="card-head border-t border-b-0 justify-end">{footer}</footer>}
      </div>
    </div>
  )
}

/* -------------------------------------------------------------------------- */
/* Errors                                                                      */
/* -------------------------------------------------------------------------- */

export function ErrorNote({ error, className = "" }: { error: unknown; className?: string }) {
  if (!error) return null

  const message = error instanceof Error ? error.message : String(error)
  const detail =
    error && typeof error === "object" && "detail" in error
      ? ((error as { detail?: string[] }).detail ?? [])
      : []

  return (
    <div
      className={`rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-800 dark:border-red-900/60 dark:bg-red-950/40 dark:text-red-300 ${className}`}
    >
      <p className="font-medium">{message}</p>
      {detail.length > 0 && (
        <ul className="mt-2 list-disc space-y-1 pl-5 text-xs">
          {detail.map((line, index) => (
            <li key={index} className="break-words">
              {line}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

export function TokenReveal({ token, note }: { token: string; note?: string }) {
  return (
    <div className="space-y-3">
      <p className="text-sm text-zinc-600 dark:text-zinc-300">
        {note ?? "Store this token now. Only its hash is kept, so it cannot be shown again."}
      </p>
      <CopyField value={token} />
      <p className="mono text-xs text-zinc-500 dark:text-zinc-400">{maskToken(token)}</p>
    </div>
  )
}
