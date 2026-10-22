import { defineConfig } from "tsdown";

export default defineConfig({
  entry: ["src/index.ts", "src/http.ts", "src/fastify.ts", "src/hono.ts", "src/cli.ts"],
  format: "esm",
  platform: "node",
  target: "node22",
  outDir: "lib",
  dts: true,
  clean: true,
  // Export paths in package.json use .js and .d.ts.
  fixedExtension: false,
});
