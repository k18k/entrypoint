import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` proxies the API to a panel backend, e.g. an SSH tunnel to the gateway's 127.0.0.1:7700.
export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist", emptyOutDir: true, assetsDir: "assets", sourcemap: false },
  server: { proxy: { "/api": process.env.PANEL_BACKEND ?? "http://127.0.0.1:7700" } },
});
