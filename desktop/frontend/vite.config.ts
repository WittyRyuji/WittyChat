import react from "@vitejs/plugin-react";
import wails from "@wailsio/runtime/plugins/vite";
import { defineConfig } from "vite";

// https://vitejs.dev/config/
export default defineConfig({
  server: {
    host: "127.0.0.1",
    port: Number(process.env.WAILS_VITE_PORT) || 9245,
    strictPort: true,
    // Dev proxy → Go server. In Wails/desktop the binary embeds dist and
    // talks to the configured server URL directly.
    // 开发代理 → Go 服务端。Wails 下由二进制内嵌 dist、直连配置的服务端地址。
    proxy: {
      "/api": "http://localhost:8080",
      "/ws": { target: "ws://localhost:8080", ws: true },
    },
  },
  plugins: [react(), wails("./bindings")],
});
