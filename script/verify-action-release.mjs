// Stamps the verify action for a stable CLI release. Run it after the release
// workflow publishes vX.Y.Z, with gh signed in:
//
//   node script/verify-action-release.mjs vX.Y.Z
//     Checks the release binaries' attestations, adds their checksums to
//     actions/verify/checksums.json as the new default, and opens that change
//     against main as a pull request from verify-stamp-vX.Y.Z.
//   node script/verify-action-release.mjs --tag vX.Y.Z
//     After that pull request merges, tags main verify-vX.Y.Z.
//
// Users pin verify-vX.Y.Z. The vX.Y.Z release commit predates its own stamp, so
// it doesn't list vX.Y.Z. Rerunning either step after a partial run is safe.

import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { parseChecksums } from "../actions/verify/lib.mjs";

export const ASSETS = [
  "linux-amd64",
  "linux-arm64",
  "darwin-amd64",
  "darwin-arm64",
  "windows-amd64.exe",
  "windows-arm64.exe",
];
export const CHECKSUMS = "actions/verify/checksums.json";

export function checkTag(tag) {
  if (!/^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(tag ?? "")) {
    throw new Error(`invalid tag '${tag}'; expected a stable release vX.Y.Z`);
  }
  return tag;
}

// newestFirst orders vX.Y.Z[-rc.N] versions, a release after its candidates.
function newestFirst(a, b) {
  const key = (t) => {
    const [, x, y, z, rc] = /^v(\d+)\.(\d+)\.(\d+)(?:-rc\.(\d+))?$/.exec(t);
    return [+x, +y, +z, rc === undefined ? Infinity : +rc];
  };
  const ka = key(a);
  const kb = key(b);
  for (let i = 0; i < ka.length; i++) if (ka[i] !== kb[i]) return kb[i] - ka[i];
  return 0;
}

// addRelease returns checksums.json text with sha256 listed under tag and tag as
// the default. text is the current file, or null. An existing entry for tag
// must match.
export function addRelease(text, tag, sha256) {
  checkTag(tag);
  if (Object.keys(sha256).length !== ASSETS.length || ASSETS.some((a) => !/^[0-9a-f]{64}$/.test(sha256[a] ?? ""))) {
    throw new Error(`checksums for ${tag} must cover ${ASSETS.join(", ")}`);
  }
  const { versions } = text === null ? { versions: {} } : parseChecksums(text);
  const old = versions[tag];
  if (old && (Object.keys(old).length !== ASSETS.length || ASSETS.some((a) => old[a] !== sha256[a]))) {
    throw new Error(`${CHECKSUMS} already lists different checksums for ${tag}`);
  }
  versions[tag] = Object.fromEntries(ASSETS.map((a) => [a, sha256[a]]));
  const sorted = Object.fromEntries(Object.keys(versions).sort(newestFirst).map((v) => [v, versions[v]]));
  return JSON.stringify({ default: tag, versions: sorted }, null, 2) + "\n";
}

// ghAPI calls `gh api`, so requests use the caller's gh credentials. A 404
// returns null.
export function ghAPI(method, path, body) {
  const args = ["api", "-X", method, path];
  if (body) args.push("--input", "-");
  const r = spawnSync("gh", args, { encoding: "utf8", input: body ? JSON.stringify(body) : undefined });
  if (r.status !== 0) {
    if (/\(HTTP 404\)/.test(r.stderr)) return null;
    throw new Error(`gh api ${method} ${path}: ${r.stderr.trim()}`);
  }
  return r.stdout ? JSON.parse(r.stdout) : {};
}

// checksumsAt returns checksums.json text at ref, or null if it doesn't exist.
async function checksumsAt(api, repo, ref) {
  const file = await api("GET", `repos/${repo}/contents/${CHECKSUMS}?ref=${ref}`);
  return file && Buffer.from(file.content, "base64").toString("utf8");
}

function defaultIn(text) {
  try {
    return parseChecksums(text).default;
  } catch {
    return null;
  }
}

// openStampPR opens or reuses the pull request that adds tag to main's
// checksums.json. verifiedSHA256 returns the attested release checksums; it only
// runs when main isn't stamped yet. Returns the pull request URL, or null if main
// already is.
export async function openStampPR({ api, repo, tag, verifiedSHA256, log = console.log }) {
  checkTag(tag);
  const main = (await api("GET", `repos/${repo}/git/ref/heads/main`)).object.sha;
  const current = await checksumsAt(api, repo, main);
  if (current !== null && defaultIn(current) === tag) {
    log(`main is already stamped for ${tag}.`);
    return null;
  }
  const stamped = addRelease(current, tag, await verifiedSHA256());

  const branch = `verify-stamp-${tag}`;
  const ref = await api("GET", `repos/${repo}/git/ref/heads/${branch}`);
  let head = ref?.object.sha;
  let commit = true;
  if (!ref) {
    await api("POST", `repos/${repo}/git/refs`, { ref: `refs/heads/${branch}`, sha: main });
    head = main;
  } else {
    // A branch from an earlier run is reused only if it changes nothing but
    // checksums.json, to exactly this stamp.
    const cmp = await api("GET", `repos/${repo}/compare/main...${head}`);
    if (cmp.ahead_by > 0) {
      const files = cmp.files.map((f) => f.filename);
      if (files.length !== 1 || files[0] !== CHECKSUMS || (await checksumsAt(api, repo, head)) !== stamped) {
        throw new Error(`branch ${branch} isn't the verified stamp for ${tag}; delete it and rerun`);
      }
      commit = false;
    }
  }
  if (commit) {
    // createCommitOnBranch commits are signed by GitHub.
    const res = await api("POST", "graphql", {
      query: `mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input: $input) { commit { oid } } }`,
      variables: {
        input: {
          branch: { repositoryNameWithOwner: repo, branchName: branch },
          message: { headline: `Stamp actions/verify for ${tag}` },
          expectedHeadOid: head,
          fileChanges: { additions: [{ path: CHECKSUMS, contents: Buffer.from(stamped).toString("base64") }] },
        },
      },
    });
    if (res.errors?.length) throw new Error(`createCommitOnBranch: ${res.errors.map((e) => e.message).join("; ")}`);
    head = res.data.createCommitOnBranch.commit.oid;
  }
  const { verification } = await api("GET", `repos/${repo}/git/commits/${head}`);
  if (!verification?.verified) throw new Error(`${branch} head ${head} is not signed`);

  // head= also matches forks' branches of the same name.
  const owner = repo.split("/")[0];
  const pulls = await api("GET", `repos/${repo}/pulls?state=open&base=main&head=${owner}:${branch}`);
  let pr = pulls.find((p) => p.head.repo?.full_name === repo && p.head.sha === head);
  pr ??= await api("POST", `repos/${repo}/pulls`, {
    title: `Stamp actions/verify for ${tag}`,
    head: branch,
    base: "main",
    body: `Adds ${tag} and its release binaries' checksums to \`${CHECKSUMS}\` as the default. After it merges, tag it with \`node script/verify-action-release.mjs --tag ${tag}\`.`,
  });
  log(`Stamp pull request: ${pr.html_url}`);
  return pr.html_url;
}

// tagRelease tags main verify-<tag> once main's checksums.json defaults to tag.
// GitHub signs merges, so main's tip is a signed commit.
export async function tagRelease({ api, repo, tag, log = console.log }) {
  checkTag(tag);
  const name = `verify-${tag}`;
  const existing = await api("GET", `repos/${repo}/git/ref/tags/${name}`);
  if (existing) {
    const sha = existing.object.sha;
    if (existing.object.type !== "commit" || defaultIn(await checksumsAt(api, repo, sha)) !== tag) {
      throw new Error(`${name} already exists at ${sha}, which isn't stamped for ${tag}`);
    }
    log(`${name} already exists at ${sha}.`);
    return sha;
  }
  const main = (await api("GET", `repos/${repo}/git/ref/heads/main`)).object.sha;
  const stamped = defaultIn(await checksumsAt(api, repo, main));
  if (stamped !== tag) {
    throw new Error(`main is stamped for ${stamped ?? "nothing"}, not ${tag}; merge its stamp pull request first`);
  }
  const { verification } = await api("GET", `repos/${repo}/git/commits/${main}`);
  if (!verification?.verified) throw new Error(`main ${main} is not signed`);
  await api("POST", `repos/${repo}/git/refs`, { ref: `refs/tags/${name}`, sha: main });
  log(`Tagged ${name} at ${main}.`);
  return main;
}

function run(cmd, args) {
  const r = spawnSync(cmd, args, { encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] });
  if (r.status !== 0) throw new Error(`${cmd} ${args.join(" ")} failed`);
}

// verifiedSHA256 downloads tag's release binaries, checks their attestations and
// returns their checksums.
async function verifiedSHA256(repo, tag) {
  const dir = await mkdtemp(join(tmpdir(), "verify-stamp-"));
  try {
    const sha256 = {};
    for (const a of ASSETS) {
      run("gh", ["release", "download", tag, "--repo", repo, "--pattern", a, "--dir", dir]);
      run("gh", [
        "attestation", "verify", join(dir, a),
        "--repo", repo,
        "--signer-workflow", `${repo}/.github/workflows/release.yml`,
        "--source-ref", `refs/tags/${tag}`,
      ]);
      sha256[a] = createHash("sha256").update(await readFile(join(dir, a))).digest("hex");
    }
    return sha256;
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const args = process.argv.slice(2);
    const tagOnly = args[0] === "--tag";
    if (tagOnly) args.shift();
    if (args.length !== 1) throw new Error("usage: node script/verify-action-release.mjs [--tag] vX.Y.Z");
    const tag = checkTag(args[0]);
    const repo = process.env.REPO || "github/gh-actions-lock";
    if (tagOnly) await tagRelease({ api: ghAPI, repo, tag });
    else await openStampPR({ api: ghAPI, repo, tag, verifiedSHA256: () => verifiedSHA256(repo, tag) });
  } catch (err) {
    console.error(`error: ${err.message}`);
    process.exitCode = 1;
  }
}
