// Logic for actions/verify. verify.mjs is the entry point; tests import this.

import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { createWriteStream, existsSync } from "node:fs";
import { chmod, mkdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";
import { setTimeout as sleep } from "node:timers/promises";

export const REPO = "github/gh-actions-lock";

export class ActionError extends Error {}

export function checkVersion(version) {
  if (!version) {
    throw new ActionError(`set the version input, or use a verify-v* release tag of ${REPO}/actions/verify`);
  }
  if (!/^v\d+\.\d+\.\d+(-rc\.\d+)?$/.test(version)) {
    throw new ActionError(`invalid version '${version}'; expected vX.Y.Z or vX.Y.Z-rc.N`);
  }
  return version;
}

const platforms = { Linux: ["linux", ""], macOS: ["darwin", ""], Windows: ["windows", ".exe"] };
const arches = { X64: "amd64", ARM64: "arm64" };

// assetFor maps RUNNER_OS and RUNNER_ARCH to a release asset name.
export function assetFor(runnerOS, runnerArch) {
  const platform = platforms[runnerOS];
  if (!platform) throw new ActionError(`unsupported runner OS '${runnerOS}'`);
  const arch = arches[runnerArch];
  if (!arch) throw new ActionError(`unsupported runner architecture '${runnerArch}'`);
  const [os, ext] = platform;
  return { asset: `${os}-${arch}${ext}`, ext };
}

// splitArgs splits the args input on any whitespace, newlines included. Nothing is
// globbed or expanded.
export function splitArgs(input) {
  return (input ?? "").split(/\s+/).filter(Boolean);
}

// selectCheck picks how to check the binary: the checksum stamped into this action
// when version is the release it was stamped with, otherwise the release's
// build attestation. checksums is the text of checksums.txt, or null.
export function selectCheck(version, checksums, asset) {
  const lines = checksums?.split(/\r?\n/) ?? [];
  const header = lines[0]?.split(/\s+/);
  const stamped = header?.[0] === "#" ? header[1] : undefined;
  if (version !== stamped) return { kind: "attestation" };
  for (const line of lines.slice(1)) {
    const [sum, name] = line.trim().split(/\s+/);
    if (name === asset) return { kind: "checksum", want: sum };
  }
  throw new ActionError(`no checksum for ${asset} in checksums.txt`);
}

// attestationEnv builds the environment for `gh attestation verify`. Attestations
// live on github.com, so a GHE.com job token must never be sent there.
export function attestationEnv(env) {
  let token = env.INPUT_TOKEN ?? "";
  if ((env.GITHUB_SERVER_URL ?? "https://github.com") !== "https://github.com") {
    token = env.GH_ACTIONS_LOCK_GITHUB_COM_TOKEN ?? "";
    if (!token) {
      throw new ActionError(
        `set GH_ACTIONS_LOCK_GITHUB_COM_TOKEN to check ${env.INPUT_VERSION}, or use the version this action was stamped with`,
      );
    }
  }
  const out = { ...env, GH_HOST: "github.com", GH_TOKEN: token };
  delete out.GITHUB_TOKEN;
  return out;
}

export async function sha256File(path) {
  return createHash("sha256").update(await readFile(path)).digest("hex");
}

// The statuses curl --retry treats as transient.
const transient = new Set([408, 429, 500, 502, 503, 504]);

// downloadFile streams url to dest, retrying network errors and transient statuses
// with exponential backoff.
export async function downloadFile(url, dest, { fetch: get = fetch, retries = 3, delayMs = 1000, wait = sleep } = {}) {
  for (let attempt = 0; ; attempt++) {
    let retry = true;
    try {
      const res = await get(url);
      if (!res.ok) {
        retry = transient.has(res.status);
        throw new Error(`HTTP ${res.status}`);
      }
      await pipeline(Readable.fromWeb(res.body), createWriteStream(dest));
      return;
    } catch (err) {
      if (!retry || attempt >= retries) throw new ActionError(`download ${url} failed: ${err.cause?.message ?? err.message}`);
      await wait(delayMs * 2 ** attempt);
    }
  }
}

// withSubdomainNoProxy adds .example.com for each bare NO_PROXY entry example.com.
// curl and node 24.22 let a bare entry match subdomains too, but node24 before that
// matches it exactly, so release-assets.githubusercontent.com would go through the
// proxy despite NO_PROXY=githubusercontent.com.
export function withSubdomainNoProxy(env) {
  const out = { ...env };
  for (const key of ["NO_PROXY", "no_proxy"]) {
    if (!out[key]) continue;
    const entries = out[key].split(",").map((e) => e.trim()).filter(Boolean);
    const extra = entries.filter((e) => !/^[.*]/.test(e) && !e.includes("/") && !entries.includes(`.${e}`)).map((e) => `.${e}`);
    out[key] = [...entries, ...extra].join(",");
  }
  return out;
}

// useEnvProxy routes fetch through the runner's HTTPS_PROXY or HTTP_PROXY, honoring
// NO_PROXY. http.setGlobalProxyFromEnv needs Node 24.14; older node24 runners
// re-run this script with NODE_USE_ENV_PROXY=1 instead. Returns true when the
// caller must re-run.
export function useEnvProxy(env, http) {
  const proxied = ["HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"].some((k) => env[k]);
  if (!proxied) return false;
  if (typeof http.setGlobalProxyFromEnv === "function") {
    http.setGlobalProxyFromEnv(env);
    return false;
  }
  return env.NODE_USE_ENV_PROXY !== "1";
}

function runGh(args, env) {
  const r = spawnSync("gh", args, { env, stdio: ["ignore", "ignore", "inherit"] });
  if (r.error?.code === "ENOENT") {
    throw new ActionError("gh is not on PATH; it's needed to check a version other than the one this action was stamped with");
  }
  return r.status === 0;
}

// install downloads the gh-actions-lock binary for this runner into RUNNER_TEMP,
// checks it and returns its path.
export async function install({ env, actionDir, download = downloadFile, gh = runGh, log = console.log }) {
  const version = checkVersion(env.INPUT_VERSION);
  const { asset, ext } = assetFor(env.RUNNER_OS, env.RUNNER_ARCH);
  if (!env.RUNNER_TEMP) throw new ActionError("RUNNER_TEMP is not set");

  const sumsPath = join(actionDir, "checksums.txt");
  const sums = existsSync(sumsPath) ? await readFile(sumsPath, "utf8") : null;
  const check = selectCheck(version, sums, asset);
  const ghEnv = check.kind === "attestation" ? attestationEnv(env) : null;

  const dir = join(env.RUNNER_TEMP, "gh-actions-lock");
  await mkdir(dir, { recursive: true });
  const bin = join(dir, `gh-actions-lock${ext}`);
  const url = `https://github.com/${REPO}/releases/download/${version}/${asset}`;
  await download(url, bin);

  if (check.kind === "checksum") {
    const got = await sha256File(bin);
    if (got !== check.want) {
      throw new ActionError(`checksum mismatch for ${asset} ${version}: got ${got}, want ${check.want}`);
    }
    log(`Verified ${asset} ${version} against the stamped checksum.`);
  } else {
    const ok = gh(
      [
        "attestation", "verify", bin,
        "--repo", REPO,
        "--signer-workflow", `${REPO}/.github/workflows/release.yml`,
        "--source-ref", `refs/tags/${version}`,
      ],
      ghEnv,
    );
    if (!ok) throw new ActionError(`attestation check failed for ${asset} ${version}`);
    log(`Verified ${asset} ${version} against its build attestation.`);
  }

  await chmod(bin, 0o755);
  return bin;
}
