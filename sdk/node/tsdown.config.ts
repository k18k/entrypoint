import { defineConfig } from "tsdown";

export default defineConfig({
  entry: ["src/index.ts", "src/http.ts", "src/cli.ts"],
  format: "esm",
  platform: "node",
  target: "node22",
  outDir: "lib",
  dts: true,
  clean: true,
  // Ścieżki eksportów w package.json używają .js i .d.ts.
  fixedExtension: false,
});
