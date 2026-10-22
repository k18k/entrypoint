// Writes package.json of a per-platform SDK package (the connector and frpc for one platform),
// an optional dependency of @k18k/entrypoint-sdk that npm installs only on matching machines.
//   node scripts/platform-package.mjs <os-cpu> <version> <dir>
import { writeFileSync } from "node:fs";

const [np, version, dir] = process.argv.slice(2);
const [os, cpu] = np.split("-");
const pkg = {
  name: `@k18k/entrypoint-sdk-${np}`,
  version,
  description: `Entrypoint connector and frpc for ${np} (used by @k18k/entrypoint-sdk)`,
  homepage: "https://github.com/k18k/entrypoint",
  license: "Apache-2.0",
  repository: { type: "git", url: "git+https://github.com/k18k/entrypoint.git", directory: "sdk/node" },
  os: [os],
  cpu: [cpu],
  files: ["bin"],
  publishConfig: { access: "public" },
};
writeFileSync(`${dir}/package.json`, JSON.stringify(pkg, null, 2) + "\n");
