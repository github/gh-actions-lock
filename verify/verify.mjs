import { spawnSync } from "node:child_process";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { install, splitArgs } from "./lib.mjs";

try {
  const bin = await install({ env: process.env, actionDir: dirname(fileURLToPath(import.meta.url)) });
  const r = spawnSync(bin, splitArgs(process.env.INPUT_ARGS), { stdio: "inherit", env: { ...process.env, GH_TOKEN: process.env.INPUT_TOKEN ?? "" } });
  if (r.error) throw r.error;
  process.exitCode = r.status ?? 1;
} catch (err) {
  console.error(`::error::${err.message}`);
  process.exitCode = 1;
}
