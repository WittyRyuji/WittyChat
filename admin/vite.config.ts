import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig({
	plugins: [react()],
	base: "/admin/",
	// Output lands inside the Go package for go:embed → single binary.
	build: { outDir: "../server/adminui/dist", emptyOutDir: true },
	server: {
		port: 9246,
		proxy: { "/api": "http://localhost:8080" },
	},
});
