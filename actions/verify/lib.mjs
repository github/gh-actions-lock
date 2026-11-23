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

const versionPattern = /^v\d+\.\d+\.\d+(-rc\.\d+)?$/;

export function checkVersion(version) {
  if (!version) {
    throw new ActionError(`set the version input, or pin an actions/verify/v* tag of ${REPO}`);
  }
  if (!versionPattern.test(version)) {
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

const isMap = (o) => o !== null && typeof o === "object" && !Array.isArray(o);

// parseChecksums reads checksums.json, which script/verify-action-release.mjs
// writes: {"default": "vX.Y.Z", "versions": {"vX.Y.Z": {"<asset>": "<sha256 hex>"}}}.
// default must be one of versions. Anything else fails.
export function parseChecksums(text) {
  let sums;
  try {
    sums = JSON.parse(text);
  } catch (err) {
    throw new ActionError(`checksums.json: ${err.message}`);
  }
  const { default: def, versions, ...extra } = isMap(sums) ? sums : {};
  const shaped =
    Object.keys(extra).length === 0 &&
    isMap(versions) &&
    Object.hasOwn(versions, def) &&
    Object.entries(versions).every(
      ([v, sha256]) =>
        versionPattern.test(v) &&
        isMap(sha256) &&
        Object.keys(sha256).length > 0 &&
        Object.entries(sha256).every(([k, h]) => /^[a-z0-9][a-z0-9.-]*$/.test(k) && /^[0-9a-f]{64}$/.test(h)),
    );
  if (!shaped) {
    throw new ActionError('checksums.json must be {"default": "vX.Y.Z", "versions": {"vX.Y.Z": {"<asset>": "<sha256 hex>"}}}');
  }
  return { default: def, versions };
}

// selectCheck picks the version to run and how to check it: its checksum when
// checksums.json lists it, otherwise its build attestation. An empty version
// runs the default. sums is parsed checksums.json, or null.
export function selectCheck(v, sums, asset) {
  v = checkVersion(v || sums?.default);
  if (!sums || !Object.hasOwn(sums.versions, v)) return { version: v, kind: "attestation" };
  const sha256 = sums.versions[v];
  if (!Object.hasOwn(sha256, asset)) throw new ActionError(`no checksum for ${asset} ${v} in checksums.json`);
  return { version: v, kind: "checksum", want: sha256[asset] };
}

// attestationEnv builds the environment for `gh attestation verify`. Attestations
// live on github.com, so a GHE.com job token must never be sent there.
export function attestationEnv(env) {
  let token = env.INPUT_TOKEN ?? "";
  if ((env.GITHUB_SERVER_URL ?? "https://github.com") !== "https://github.com") {
    token = env.GH_ACTIONS_LOCK_GITHUB_COM_TOKEN ?? "";
    if (!token) {
      throw new ActionError(
        `set GH_ACTIONS_LOCK_GITHUB_COM_TOKEN to check ${env.INPUT_VERSION}, or leave version empty`,
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
    throw new ActionError("gh is not on PATH; it's needed to check a version other than the one in checksums.json");
  }
  return r.status === 0;
}

// install downloads the gh-actions-lock binary for this runner into RUNNER_TEMP,
// checks it and returns its path.
export async function install({ env, actionDir, download = downloadFile, gh = runGh, log = console.log }) {
  const { asset, ext } = assetFor(env.RUNNER_OS, env.RUNNER_ARCH);
  if (!env.RUNNER_TEMP) throw new ActionError("RUNNER_TEMP is not set");

  const sumsPath = join(actionDir, "checksums.json");
  const sums = existsSync(sumsPath) ? parseChecksums(await readFile(sumsPath, "utf8")) : null;
  const check = selectCheck(env.INPUT_VERSION, sums, asset);
  const { version } = check;
  const ghEnv = check.kind === "attestation" ? attestationEnv({ ...env, INPUT_VERSION: version }) : null;

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
    log(`Verified ${asset} ${version} against checksums.json.`);
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
