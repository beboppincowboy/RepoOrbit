import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { tanstackStart } from "@tanstack/react-start/plugin/vite";

// Same-origin API: the browser talks to this server; these paths are forwarded to the Go API.
// The production server (server.mjs) reads the same API_ORIGIN at runtime.
const api = process.env.API_ORIGIN ?? "http://127.0.0.1:8080";
const proxy = { "/auth": { target: api }, "/api": { target: api }, "/graphql": { target: api } };

export default defineConfig({
  resolve: { tsconfigPaths: true },
  server: { proxy },
  plugins: [tanstackStart({ spa: { enabled: true } }), react(), tailwindcss()],
});
