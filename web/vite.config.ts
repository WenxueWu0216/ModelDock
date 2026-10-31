/// <reference types="vitest" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In dev, the Vite server proxies API calls to the Go server so the console
// runs same-origin, exactly as it does when embedded in the binary. Set
// VITE_API_BASE (e.g. http://localhost:8080) to call the API cross-origin
// instead; the API's -cors-origins allows http://localhost:5173 by default.
const target = process.env.MODELDOCK_API ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target, changeOrigin: true },
      "/graphql": { target, changeOrigin: true },
    },
  },
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    css: false,
  },
});
