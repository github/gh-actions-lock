import { spawnSync } from "node:child_process";
import http from "node:http";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { install, splitArgs, useEnvProxy, withSubdomainNoProxy } from "./lib.mjs";

const run = (cmd, args, env) => {
  const r = spawnSync(cmd, args, { stdio: "inherit", env });
  if (r.error) throw r.error;
  return r.status ?? 1;
};

try {
  const env = withSubdomainNoProxy(process.env);
  if (useEnvProxy(env, http)) {
    process.exitCode = run(process.execPath, [fileURLToPath(import.meta.url)], { ...env, NODE_USE_ENV_PROXY: "1" });
  } else {
    const bin = await install({ env, actionDir: dirname(fileURLToPath(import.meta.url)) });
    process.exitCode = run(bin, splitArgs(env.INPUT_ARGS), { ...env, GH_TOKEN: env.INPUT_TOKEN ?? "" });
  }
} catch (err) {
  console.error(`::error::${err.message}`);
  process.exitCode = 1;
}
