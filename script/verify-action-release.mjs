// Publishes verify-<tag> for a CLI release: checks the release binaries'
// attestations, stamps actions/verify with the tag and their checksums, and tags
// a GitHub-signed commit on top of the release commit.
//
// Usage (release workflow, with GH_TOKEN and REPO set):
//   node script/verify-action-release.mjs <vX.Y.Z[-rc.N]>
// To restamp the working tree only, for example to move main to a newer release:
//   node script/verify-action-release.mjs --stamp-only <vX.Y.Z[-rc.N]>

import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const ASSETS = [
  "linux-amd64",
  "linux-arm64",
  "darwin-amd64",
  "darwin-arm64",
  "windows-amd64.exe",
  "windows-arm64.exe",
];
export const ACTION = "actions/verify/action.yml";
export const CHECKSUMS = "actions/verify/checksums.txt";
const MARKER = "# stamped by script/verify-action-release.mjs";
const STAMP = /^(\s*default: )\S+( # stamped by script\/verify-action-release\.mjs)$/m;

export function checkTag(tag) {
  if (!/^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-rc\.[1-9]\d*)?$/.test(tag ?? "")) {
    throw new Error(`invalid tag '${tag}'; expected vX.Y.Z or vX.Y.Z-rc.N`);
  }
  return tag;
}

// stamp defaults the action's version input to tag and writes checksums.txt for
// the release binaries in assetsDir, replacing any earlier stamp. The marker
// comment stays so main and every verify-v* tag can be restamped the same way.
export async function stamp({ root, tag, assetsDir }) {
  checkTag(tag);
  const actionPath = join(root, ACTION);
  const action = await readFile(actionPath, "utf8");
  if (action.split(MARKER).length !== 2 || !STAMP.test(action)) {
    throw new Error(`${ACTION} must contain exactly one line like: default: vX.Y.Z ${MARKER}`);
  }
  const lines = [`# ${tag}`]; // script/release reads this header
  for (const a of ASSETS) {
    const path = join(assetsDir, a);
    if (!existsSync(path)) throw new Error(`missing release asset ${path}`);
    lines.push(`${createHash("sha256").update(await readFile(path)).digest("hex")}  ${a}`);
  }
  await writeFile(join(root, CHECKSUMS), lines.join("\n") + "\n");
  await writeFile(actionPath, action.replace(STAMP, `$1${tag}$2`));
}

// publish commits files on top of head through the Git Data API, so GitHub signs
// the commit as the tag ruleset requires, and tags it verify-<tag>.
// github returns a caller for repo's REST API. With allow404, a 404 returns null.
function github({ api, token, repo, fetch: fetchFn = fetch }) {
  return async (method, path, body, { allow404 = false } = {}) => {
    const res = await fetchFn(`${api}/repos/${repo}/${path}`, {
      method,
      headers: {
        authorization: `Bearer ${token}`,
        accept: "application/vnd.github+json",
        "content-type": "application/json",
      },
      body: body && JSON.stringify(body),
    });
    if (allow404 && res.status === 404) return null;
    if (!res.ok) throw new Error(`${method} ${path}: HTTP ${res.status} ${await res.text()}`);
    return res.json();
  };
}

// stampedFor returns the tag root's action and checksums.txt are both stamped
// for, or null if they disagree.
export async function stampedFor(root) {
  const action = (await readFile(join(root, ACTION), "utf8")).match(STAMP)?.[0].match(/default: (\S+)/)[1];
  const header = existsSync(join(root, CHECKSUMS))
    ? (await readFile(join(root, CHECKSUMS), "utf8")).split("\n", 1)[0]
    : "";
  return action && header === `# ${action}` ? action : null;
}

// published returns the commit verify-<tag> points at, or null if the tag
// doesn't exist. A rerun only skips the tag if it's a signed commit directly on
// head whose tree is exactly tree, the freshly stamped one; anything else fails.
export async function published({ api, token, repo, tag, head, tree, fetch: fetchFn = fetch }) {
  const call = github({ api, token, repo, fetch: fetchFn });
  const ref = await call("GET", `git/ref/tags/verify-${tag}`, undefined, { allow404: true });
  if (!ref) return null;
  const sha = ref.object?.sha;
  const commit = ref.object?.type === "commit" ? await call("GET", `git/commits/${sha}`) : null;
  if (
    commit?.parents?.length !== 1 ||
    commit.parents[0].sha !== head ||
    commit.tree?.sha !== tree ||
    !commit.verification?.verified
  ) {
    throw new Error(`verify-${tag} already exists at ${sha}, which is not this stamp signed on ${head}`);
  }
  return sha;
}

// publish commits files on top of head through the Git Data API, so GitHub signs
// the commit as the tag ruleset requires, and tags it verify-<tag>.
export async function publish({ api, token, repo, tag, head, baseTree, files, fetch: fetchFn = fetch }) {
  const call = github({ api, token, repo, fetch: fetchFn });

  const tree = [];
  for (const { path, content } of files) {
    const blob = await call("POST", "git/blobs", { content: Buffer.from(content).toString("base64"), encoding: "base64" });
    tree.push({ path, mode: "100644", type: "blob", sha: blob.sha });
  }
  const { sha: treeSha } = await call("POST", "git/trees", { base_tree: baseTree, tree });
  const { sha: commit } = await call("POST", "git/commits", {
    message: `Stamp actions/verify for ${tag}`,
    tree: treeSha,
    parents: [head],
  });
  const { verification } = await call("GET", `git/commits/${commit}`);
  if (!verification?.verified) throw new Error(`stamp commit ${commit} is not signed`);
  await call("POST", "git/refs", { ref: `refs/tags/verify-${tag}`, sha: commit });
  return commit;
}

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { encoding: "utf8", stdio: ["ignore", "pipe", "inherit"], ...opts });
  if (r.status !== 0) throw new Error(`${cmd} ${args.join(" ")} failed`);
  return r.stdout.trim();
}

async function main(tag, { stampOnly = false } = {}) {
  checkTag(tag);
  const { REPO: repo = stampOnly ? "github/gh-actions-lock" : undefined, GH_TOKEN: token, GITHUB_API_URL: api = "https://api.github.com" } = process.env;
  if (!repo || (!token && !stampOnly)) throw new Error("set REPO and GH_TOKEN");
  const root = resolve(fileURLToPath(import.meta.url), "../..");

  const head = run("git", ["rev-parse", "HEAD"], { cwd: root });
  // Each step skips when a previous run already did it.
  if (stampOnly && (await stampedFor(root)) === tag) {
    console.log(`${ACTION} and ${CHECKSUMS} are already stamped for ${tag}`);
    return;
  }
  const assetsDir = await mkdtemp(join(process.env.RUNNER_TEMP ?? tmpdir(), "assets-"));
  for (const a of ASSETS) {
    run("gh", ["release", "download", tag, "--repo", repo, "--pattern", a, "--dir", assetsDir]);
    run("gh", [
      "attestation", "verify", join(assetsDir, a),
      "--repo", repo,
      "--signer-workflow", `${repo}/.github/workflows/release.yml`,
      "--source-ref", `refs/tags/${tag}`,
    ]);
  }

  await stamp({ root, tag, assetsDir });
  if (stampOnly) {
    console.log(`Stamped ${ACTION} and ${CHECKSUMS} for ${tag}`);
    return;
  }
  // The tree head would have with the stamp, from a scratch index.
  const env = { ...process.env, GIT_INDEX_FILE: join(assetsDir, "index") };
  run("git", ["read-tree", "HEAD"], { cwd: root, env });
  run("git", ["add", "--", ACTION, CHECKSUMS], { cwd: root, env });
  const tree = run("git", ["write-tree"], { cwd: root, env });
  const done = await published({ api, token, repo, tag, head, tree });
  if (done) {
    console.log(`verify-${tag} already exists at ${done}`);
    return;
  }
  const files = [];
  for (const path of [ACTION, CHECKSUMS]) files.push({ path, content: await readFile(join(root, path)) });
  const commit = await publish({
    api,
    token,
    repo,
    tag,
    head,
    baseTree: run("git", ["rev-parse", "HEAD^{tree}"], { cwd: root }),
    files,
  });
  console.log(`Tagged verify-${tag} at ${commit}`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const args = process.argv.slice(2);
    const stampOnly = args[0] === "--stamp-only";
    if (stampOnly) args.shift();
    if (args.length !== 1) throw new Error("usage: node script/verify-action-release.mjs [--stamp-only] <tag>");
    await main(args[0], { stampOnly });
  } catch (err) {
    console.error(`::error::${err.message}`);
    process.exitCode = 1;
  }
}
