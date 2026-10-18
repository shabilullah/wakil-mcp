import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react"
import { ApiError } from "../api"

export type ToastKind = "success" | "error" | "info"

export interface Toast {
  id: number
  kind: ToastKind
  title: string
  detail: string[]
}

interface ToastApi {
  push(kind: ToastKind, title: string, detail?: string[]): void
  success(title: string, detail?: string[]): void
  info(title: string, detail?: string[]): void
  /** report turns a thrown value into a readable failure toast. */
  report(error: unknown, context?: string): void
}

const ToastContext = createContext<ToastApi | null>(null)

/** useToast reaches the toast stack from any component. */
export function useToast(): ToastApi {
  const api = useContext(ToastContext)
  if (!api) throw new Error("useToast was called outside the provider")
  return api
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const nextID = useRef(1)

  const remove = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id))
  }, [])

  const push = useCallback(
    (kind: ToastKind, title: string, detail: string[] = []) => {
      const id = nextID.current++
      setToasts((current) => [...current.slice(-4), { id, kind, title, detail }])

      // Failures stay longer: they usually carry something to read.
      const life = kind === "error" ? 9000 : 4000
      window.setTimeout(() => remove(id), life)
    },
    [remove],
  )

  const api = useMemo<ToastApi>(() => {
    return {
      push,
      success: (title, detail) => push("success", title, detail),
      info: (title, detail) => push("info", title, detail),
      report: (error, context) => {
        if (error instanceof ApiError) {
          push("error", context ? `${context}: ${error.message}` : error.message, error.detail)
          return
        }
        const message = error instanceof Error ? error.message : String(error)
        push("error", context ? `${context}: ${message}` : message)
      },
    }
  }, [push])

  const styles: Record<ToastKind, string> = {
    success:
      "border-emerald-200 bg-white text-zinc-900 dark:border-emerald-900/70 dark:bg-zinc-900 dark:text-zinc-100",
    error:
      "border-red-200 bg-white text-zinc-900 dark:border-red-900/70 dark:bg-zinc-900 dark:text-zinc-100",
    info: "border-zinc-200 bg-white text-zinc-900 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-100",
  }

  const dots: Record<ToastKind, string> = {
    success: "bg-emerald-500",
    error: "bg-red-500",
    info: "bg-sky-500",
  }

  return (
    <ToastContext.Provider value={api}>
      {children}

      <div className="pointer-events-none fixed right-4 bottom-4 z-[60] flex w-[min(24rem,calc(100vw-2rem))] flex-col gap-2">
        {toasts.map((toast) => (
          <div
            key={toast.id}
            role="status"
            className={`pointer-events-auto flex items-start gap-3 rounded-xl border p-3 shadow-lg ${styles[toast.kind]}`}
          >
            <span className={`mt-1.5 size-2 shrink-0 rounded-full ${dots[toast.kind]}`} />
            <div className="min-w-0 flex-1">
              <p className="text-sm font-medium break-words">{toast.title}</p>
              {toast.detail.length > 0 && (
                <ul className="mt-1 list-disc space-y-0.5 pl-4 text-xs text-zinc-500 dark:text-zinc-400">
                  {toast.detail.map((line, index) => (
                    <li key={index} className="break-words">
                      {line}
                    </li>
                  ))}
                </ul>
              )}
            </div>
            <button
              type="button"
              className="btn btn-ghost btn-sm shrink-0"
              onClick={() => remove(toast.id)}
              aria-label="Dismiss"
            >
              ×
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}
