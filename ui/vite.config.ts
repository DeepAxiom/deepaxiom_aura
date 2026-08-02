import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The production build is embedded into the kernel binary (go:embed),
// so the app ships inside the single `aura` executable.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../kernel/internal/gateway/ui/dist",
    emptyOutDir: true,
  },
  server: {
    port: 3000,
    proxy: {
      "/v1": { target: "http://localhost:9080", ws: true },
      "/healthz": { target: "http://localhost:9080" },
    },
  },
});
