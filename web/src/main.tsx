import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { App } from "./App"
import { ToastProvider } from "./components/Toast"
import "./index.css"

const host = document.getElementById("root")
if (!host) throw new Error("the root element is missing from index.html")

createRoot(host).render(
  <StrictMode>
    <ToastProvider>
      <App />
    </ToastProvider>
  </StrictMode>,
)
