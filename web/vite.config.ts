import { fileURLToPath, URL } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The SPA ships inside the server: it builds into the server's static files.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  build: {
    outDir: "../server/Casebox.Server/wwwroot",
    emptyOutDir: true,
  },
  server: {
    proxy: { "/api": "http://localhost:8080" },
  },
});
