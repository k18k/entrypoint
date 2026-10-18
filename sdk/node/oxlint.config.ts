import { defineConfig } from "oxlint";

export default defineConfig({
  plugins: ["typescript", "unicorn", "import", "node", "promise"],
  categories: {
    correctness: "error",
    suspicious: "warn",
  },
  env: { node: true, es2024: true },
  ignorePatterns: ["lib/**"],
  rules: {
    "no-console": "off",
    "typescript/no-explicit-any": "error",
    "typescript/consistent-type-imports": "error",
    "import/no-cycle": "error",
    "unicorn/prefer-node-protocol": "error",
    // Callbacki .then() reagujące na koniec procesu celowo nic nie zwracają.
    "promise/always-return": "off",
  },
});
