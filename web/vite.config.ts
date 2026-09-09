import { defineConfig } from "vite";
import preact from "@preact/preset-vite";

// The build output lands in internal/web/dist, which the Go binary embeds via
// go:embed (see internal/web/web.go). `make ui` runs this build. The dev
// server proxies /api to the Go server started with `make serve`.
export default defineConfig({
	plugins: [preact()],
	build: {
		outDir: "../internal/web/dist",
		emptyOutDir: true,
		target: "es2022",
	},
	server: {
		proxy: {
			"/api": "http://127.0.0.1:8080",
		},
	},
});
