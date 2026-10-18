import { useEffect, useRef } from "react"
import { EditorView, keymap } from "@codemirror/view"
import { Compartment, EditorState } from "@codemirror/state"
import { basicSetup } from "codemirror"
import { json, jsonParseLinter } from "@codemirror/lang-json"
import { linter, lintGutter } from "@codemirror/lint"
import { oneDark } from "@codemirror/theme-one-dark"

export interface JsonEditorProps {
  value: string
  onChange: (next: string) => void
  /** onSave is called for Ctrl/Cmd+S. */
  onSave?: () => void
  dark: boolean
  readOnly?: boolean
  className?: string
}

/**
 * JsonEditor is a CodeMirror instance configured for JSON: syntax
 * highlighting, a linter that reports parse errors inline, bracket matching and
 * undo/redo.
 *
 * The editor owns its document after mount. The value prop is only pushed back
 * in when it differs from what the editor holds, which is what makes a
 * "format" or "revert" button work without fighting the user's typing.
 */
export function JsonEditor({
  value,
  onChange,
  onSave,
  dark,
  readOnly = false,
  className = "",
}: JsonEditorProps) {
  const host = useRef<HTMLDivElement | null>(null)
  const view = useRef<EditorView | null>(null)

  const themeSlot = useRef(new Compartment())
  const editableSlot = useRef(new Compartment())

  // The callbacks change on every render, so they are read through refs rather
  // than baked into the extensions.
  const changeHandler = useRef(onChange)
  const saveHandler = useRef(onSave)
  changeHandler.current = onChange
  saveHandler.current = onSave

  useEffect(() => {
    if (!host.current) return

    const state = EditorState.create({
      doc: value,
      extensions: [
        basicSetup,
        json(),
        linter(jsonParseLinter()),
        lintGutter(),
        EditorView.lineWrapping,
        EditorState.tabSize.of(2),
        themeSlot.current.of(dark ? oneDark : []),
        editableSlot.current.of([
          EditorView.editable.of(!readOnly),
          EditorState.readOnly.of(readOnly),
        ]),
        keymap.of([
          {
            key: "Mod-s",
            preventDefault: true,
            run: () => {
              saveHandler.current?.()
              return true
            },
          },
        ]),
        EditorView.updateListener.of((update) => {
          if (update.docChanged) changeHandler.current(update.state.doc.toString())
        }),
      ],
    })

    const instance = new EditorView({ state, parent: host.current })
    view.current = instance

    return () => {
      instance.destroy()
      view.current = null
    }
    // Mount-only on purpose: see the doc comment.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    const instance = view.current
    if (!instance) return

    const current = instance.state.doc.toString()
    if (current === value) return

    instance.dispatch({
      changes: { from: 0, to: current.length, insert: value },
    })
  }, [value])

  useEffect(() => {
    view.current?.dispatch({
      effects: themeSlot.current.reconfigure(dark ? oneDark : []),
    })
  }, [dark])

  useEffect(() => {
    view.current?.dispatch({
      effects: editableSlot.current.reconfigure([
        EditorView.editable.of(!readOnly),
        EditorState.readOnly.of(readOnly),
      ]),
    })
  }, [readOnly])

  return <div ref={host} className={`h-full overflow-hidden ${className}`} />
}
