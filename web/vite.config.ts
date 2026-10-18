import { defineConfig } from "vite"
import react from "@vitejs/plugin-react"
import tailwindcss from "@tailwindcss/vite"

// The build lands in the directory internal/server/ui.go embeds, so a plain Go
// build always ships the app that was checked in.
//
// Point WAKIL_DEV_TARGET at a running daemon to develop against it: npm run dev
// serves the app with hot reload and forwards the API and MCP endpoints.
const target = process.env.WAKIL_DEV_TARGET ?? "http://127.0.0.1:8765"

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../internal/server/webui/dist",
    emptyOutDir: true,
    target: "es2022",
    sourcemap: false,
    rolldownOptions: {
      output: {
        // CodeMirror is only fetched when the Config tab is opened, so keeping
        // it in its own chunk keeps the first paint light.
        codeSplitting: {
          groups: [
            { name: "codemirror", test: /node_modules[\\/]@?codemirror|node_modules[\\/]lezer|node_modules[\\/]crelt|node_modules[\\/]style-mod|node_modules[\\/]w3c-keyname/ },
            { name: "react", test: /node_modules[\\/]react/ },
          ],
        },
      },
    },
  },
  server: {
    port: 5273,
    strictPort: false,
    proxy: {
      "/api": { target, changeOrigin: true },
      "/healthz": { target, changeOrigin: true },
      "/mcp": { target, changeOrigin: true },
    },
  },
})
