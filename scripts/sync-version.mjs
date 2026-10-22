// Copies the version that `changeset version` wrote to the root package.json
// to every other place the release checks: versions.json, install.sh and the SDK.
import { readFileSync, writeFileSync } from "node:fs";

const root = new URL("../", import.meta.url);
const read = (p) => readFileSync(new URL(p, root), "utf8");
const write = (p, s) => writeFileSync(new URL(p, root), s);
const edit = (p, re, to) => {
  const s = read(p);
  if (!re.test(s)) throw new Error(`${p}: version not found`);
  write(p, s.replace(re, to));
};

const { version } = JSON.parse(read("package.json"));

edit("versions.json", /("entrypoint":\s*")[^"]*"/, `$1${version}"`);
for (const p of ["scripts/install.sh", "scripts/install-connector.sh"]) {
  edit(p, /(ENTRYPOINT_VERSION:-)[^}]*\}/, `$1${version}}`);
}
for (const p of ["sdk/node/package.json", "sdk/node/package-lock.json"]) {
  const j = JSON.parse(read(p));
  j.version = version;
  for (const k of Object.keys(j.optionalDependencies ?? {})) j.optionalDependencies[k] = version;
  if (j.packages?.[""]) j.packages[""].version = version;
  write(p, JSON.stringify(j, null, 2) + "\n");
}
console.log(`synced ${version}`);
