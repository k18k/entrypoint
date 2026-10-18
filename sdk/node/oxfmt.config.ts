import { defineConfig } from "oxfmt";

export default defineConfig({
  printWidth: 100,
  semi: true,
  singleQuote: false,
  trailingComma: "all",
  ignorePatterns: ["lib/**", "package-lock.json", "connector.json"],
});
