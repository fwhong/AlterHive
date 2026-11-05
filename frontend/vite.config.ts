import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/manager": {
        target: "http://127.0.0.1:8110",
        changeOrigin: true,
        rewrite: path => path.replace(/^\/manager/, "")
      },
      "/api": {
        target: "http://127.0.0.1:8000",
        changeOrigin: true
      },
      "/healthz": {
        target: "http://127.0.0.1:8000",
        changeOrigin: true
      }
    }
  }
});
