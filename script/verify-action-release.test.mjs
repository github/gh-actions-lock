import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, test } from "node:test";
import { parseChecksums } from "../verify/lib.mjs";
import { ASSETS, CHECKSUMS, addRelease, checkTag, openStampPR, tagRelease } from "./verify-action-release.mjs";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const repo = "github/gh-actions-lock";
const sums = (c) => Object.fromEntries(ASSETS.map((a) => [a, c.repeat(64)]));
const file = (o) => JSON.stringify(o, null, 2) + "\n";
const quiet = () => {};

describe("checkTag", () => {
  for (const t of ["v1.2.3", "v0.0.1"]) test(`accepts ${t}`, () => assert.equal(checkTag(t), t));
  for (const t of ["1.2.3", "v1.2.3;x", "v01.2.3", "v1.2.3-rc.1", "verify/v1.2.3", undefined]) {
    test(`rejects ${t}`, () => assert.throws(() => checkTag(t), /invalid tag/));
  }
});

describe("checked-in checksums.json", () => {
  test("every version covers every asset, the default is the newest, and action.yml has no version default", async () => {
    const text = await readFile(join(repoRoot, CHECKSUMS), "utf8");
    const { default: def, versions } = parseChecksums(text);
    for (const [v, entry] of Object.entries(versions)) assert.deepEqual(Object.keys(entry), ASSETS, v);
    assert.equal(Object.keys(versions)[0], def);
    assert.equal(addRelease(text, def, versions[def]), text);
    const action = await readFile(join(repoRoot, "action.yml"), "utf8");
    assert.doesNotMatch(action.slice(action.indexOf("  version:"), action.indexOf("  args:")), /default:/);
  });
});

describe("addRelease", () => {
  const base = file({ default: "v1.2.3", versions: { "v1.2.3": sums("a"), "v1.2.2": sums("b") } });
  test("adds the release as the default and keeps the others", () => {
    assert.deepEqual(JSON.parse(addRelease(base, "v1.3.0", sums("c"))), {
      default: "v1.3.0",
      versions: { "v1.3.0": sums("c"), "v1.2.3": sums("a"), "v1.2.2": sums("b") },
    });
  });
  test("starts a file from nothing", () => {
    assert.deepEqual(JSON.parse(addRelease(null, "v1.0.0", sums("a"))), { default: "v1.0.0", versions: { "v1.0.0": sums("a") } });
  });
  test("lists versions newest first, whatever the order they were added in", () => {
    const out = JSON.parse(addRelease(base, "v1.2.10", sums("c")));
    assert.deepEqual(Object.keys(out.versions), ["v1.2.10", "v1.2.3", "v1.2.2"]);
  });
  test("re-adding an identical entry only moves the default", () => {
    assert.equal(JSON.parse(addRelease(base, "v1.2.2", sums("b"))).default, "v1.2.2");
    assert.equal(addRelease(base, "v1.2.3", sums("a")), base);
  });
  test("fails on an existing entry that differs", () => {
    assert.throws(() => addRelease(base, "v1.2.2", sums("c")), /already lists different checksums for v1.2.2/);
  });
  test("fails unless the checksums cover exactly the release assets", () => {
    const { "linux-amd64": _, ...missing } = sums("c");
    assert.throws(() => addRelease(base, "v1.3.0", missing), /must cover/);
    assert.throws(() => addRelease(base, "v1.3.0", { ...sums("c"), extra: "c".repeat(64) }), /must cover/);
    assert.throws(() => addRelease(base, "v1.3.0", { ...sums("c"), "linux-amd64": "C".repeat(64) }), /must cover/);
  });
  test("fails on a malformed file", () => {
    assert.throws(() => addRelease('{"version": "v1.2.3", "sha256": {}}', "v1.3.0", sums("c")));
  });
});

// fakeAPI answers "METHOD path" from routes, which are values or functions of
// the request body, and records every call. Unrouted requests fail the test.
function fakeAPI(routes) {
  const calls = [];
  const api = async (method, path, body) => {
    const key = `${method} ${path}`;
    calls.push(body === undefined ? key : [key, body]);
    if (!(key in routes)) assert.fail(`unexpected ${key}`);
    const r = routes[key];
    return typeof r === "function" ? r(body) : structuredClone(r);
  };
  return { api, calls, keys: () => calls.map((c) => (Array.isArray(c) ? c[0] : c)) };
}
const contents = (text) => ({ content: Buffer.from(text).toString("base64") });
const signed = (verified = true) => ({ verification: { verified } });

describe("openStampPR", () => {
  const onMain = file({ default: "v1.2.3", versions: { "v1.2.3": sums("a") } });
  const stamped = addRelease(onMain, "v1.3.0", sums("c"));
  const branch = "verify-stamp-v1.3.0";
  const base = {
    [`GET repos/${repo}/git/ref/heads/main`]: { object: { sha: "main1" } },
    [`GET repos/${repo}/contents/${CHECKSUMS}?ref=main1`]: contents(onMain),
    [`GET repos/${repo}/git/commits/new1`]: signed(),
    [`GET repos/${repo}/pulls?state=open&base=main&head=github:${branch}`]: [],
    [`POST repos/${repo}/pulls`]: { html_url: "https://github.com/github/gh-actions-lock/pull/9" },
  };
  const committed = { data: { createCommitOnBranch: { commit: { oid: "new1" } } } };
  const open = (api, verified = async () => sums("c")) =>
    openStampPR({ api, repo, tag: "v1.3.0", verifiedSHA256: verified, log: quiet });

  test("does nothing when main is already stamped", async () => {
    const f = fakeAPI({ ...base, [`GET repos/${repo}/contents/${CHECKSUMS}?ref=main1`]: contents(stamped) });
    assert.equal(await open(f.api, () => assert.fail("should not download")), null);
  });

  test("branches off main, commits the stamp through the signing API and opens the pull request", async () => {
    const f = fakeAPI({
      ...base,
      [`GET repos/${repo}/git/ref/heads/${branch}`]: null,
      [`POST repos/${repo}/git/refs`]: {},
      "POST graphql": committed,
    });
    assert.equal(await open(f.api), "https://github.com/github/gh-actions-lock/pull/9");
    const call = (k) => f.calls.find((c) => Array.isArray(c) && c[0] === k)[1];
    assert.deepEqual(call(`POST repos/${repo}/git/refs`), { ref: `refs/heads/${branch}`, sha: "main1" });
    const input = call("POST graphql").variables.input;
    assert.equal(input.expectedHeadOid, "main1");
    assert.deepEqual(input.branch, { repositoryNameWithOwner: repo, branchName: branch });
    assert.deepEqual(input.fileChanges.additions, [{ path: CHECKSUMS, contents: Buffer.from(stamped).toString("base64") }]);
    assert.equal(call(`POST repos/${repo}/pulls`).head, branch);
  });

  test("commits onto a leftover branch that has no commits of its own", async () => {
    const f = fakeAPI({
      ...base,
      [`GET repos/${repo}/git/ref/heads/${branch}`]: { object: { sha: "main1" } },
      [`GET repos/${repo}/compare/main...main1`]: { ahead_by: 0, files: [] },
      "POST graphql": committed,
    });
    await open(f.api);
    assert.ok(!f.keys().includes(`POST repos/${repo}/git/refs`));
    assert.ok(f.keys().includes("POST graphql"));
  });

  const leftover = (files, text, pulls = []) => ({
    ...base,
    [`GET repos/${repo}/git/ref/heads/${branch}`]: { object: { sha: "new1" } },
    [`GET repos/${repo}/compare/main...new1`]: { ahead_by: 1, files: files.map((filename) => ({ filename })) },
    [`GET repos/${repo}/contents/${CHECKSUMS}?ref=new1`]: contents(text),
    [`GET repos/${repo}/pulls?state=open&base=main&head=github:${branch}`]: pulls,
  });

  test("reuses a branch and pull request from an earlier run", async () => {
    const pr = { html_url: "https://github.com/github/gh-actions-lock/pull/8", head: { sha: "new1", repo: { full_name: repo } } };
    const f = fakeAPI(leftover([CHECKSUMS], stamped, [pr]));
    assert.equal(await open(f.api), pr.html_url);
    assert.ok(!f.keys().some((k) => k.startsWith("POST")));
  });

  test("opens its own pull request instead of reusing a fork's or a stale one", async () => {
    const fork = { html_url: "fork", head: { sha: "new1", repo: { full_name: "someone/gh-actions-lock" } } };
    const stale = { html_url: "stale", head: { sha: "old", repo: { full_name: repo } } };
    const f = fakeAPI(leftover([CHECKSUMS], stamped, [fork, stale]));
    assert.equal(await open(f.api), "https://github.com/github/gh-actions-lock/pull/9");
  });

  const tampered = {
    "different checksums": [[CHECKSUMS], addRelease(onMain, "v1.3.0", sums("d"))],
    "an extra file": [[CHECKSUMS, "action.yml"], stamped],
  };
  for (const [name, [files, text]] of Object.entries(tampered)) {
    test(`refuses a leftover branch with ${name}`, async () => {
      await assert.rejects(open(fakeAPI(leftover(files, text)).api), /isn't the verified stamp for v1.3.0; delete it/);
    });
  }

  test("fails when the commit isn't signed", async () => {
    const f = fakeAPI({
      ...base,
      [`GET repos/${repo}/git/ref/heads/${branch}`]: null,
      [`POST repos/${repo}/git/refs`]: {},
      "POST graphql": committed,
      [`GET repos/${repo}/git/commits/new1`]: signed(false),
    });
    await assert.rejects(open(f.api), /is not signed/);
    assert.ok(!f.keys().includes(`POST repos/${repo}/pulls`));
  });

  test("surfaces GraphQL errors", async () => {
    const f = fakeAPI({
      ...base,
      [`GET repos/${repo}/git/ref/heads/${branch}`]: null,
      [`POST repos/${repo}/git/refs`]: {},
      "POST graphql": { errors: [{ message: "Expected branch to point to main1" }] },
    });
    await assert.rejects(open(f.api), /createCommitOnBranch: Expected branch to point to main1/);
  });

  test("fails when the release's checksums differ from an existing entry", async () => {
    const conflict = fakeAPI({
      ...base,
      [`GET repos/${repo}/contents/${CHECKSUMS}?ref=main1`]: contents(
        file({ default: "v1.2.3", versions: { "v1.3.0": sums("e"), "v1.2.3": sums("a") } }),
      ),
    });
    await assert.rejects(open(conflict.api), /already lists different checksums/);
  });
});

describe("tagRelease", () => {
  const stamped = file({ default: "v1.3.0", versions: { "v1.3.0": sums("c") } });
  const base = {
    [`GET repos/${repo}/git/ref/tags/verify/v1.3.0`]: null,
    [`GET repos/${repo}/git/ref/heads/main`]: { object: { sha: "main2" } },
    [`GET repos/${repo}/contents/${CHECKSUMS}?ref=main2`]: contents(stamped),
    [`GET repos/${repo}/git/commits/main2`]: signed(),
    [`POST repos/${repo}/git/refs`]: {},
  };
  const tag = (api) => tagRelease({ api, repo, tag: "v1.3.0", log: quiet });

  test("tags main once its stamp has merged", async () => {
    const f = fakeAPI(base);
    assert.equal(await tag(f.api), "main2");
    assert.deepEqual(f.calls.at(-1), [`POST repos/${repo}/git/refs`, { ref: "refs/tags/verify/v1.3.0", sha: "main2" }]);
  });

  test("refuses until main is stamped for the release", async () => {
    const f = fakeAPI({
      ...base,
      [`GET repos/${repo}/contents/${CHECKSUMS}?ref=main2`]: contents(file({ default: "v1.2.3", versions: { "v1.2.3": sums("a") } })),
    });
    await assert.rejects(tag(f.api), /main is stamped for v1.2.3, not v1.3.0; merge its stamp pull request first/);
  });

  test("refuses an unsigned main", async () => {
    const f = fakeAPI({ ...base, [`GET repos/${repo}/git/commits/main2`]: signed(false) });
    await assert.rejects(tag(f.api), /main main2 is not signed/);
    assert.ok(!f.keys().includes(`POST repos/${repo}/git/refs`));
  });

  test("accepts an existing tag on a stamped commit", async () => {
    const f = fakeAPI({
      [`GET repos/${repo}/git/ref/tags/verify/v1.3.0`]: { object: { sha: "old2", type: "commit" } },
      [`GET repos/${repo}/contents/${CHECKSUMS}?ref=old2`]: contents(stamped),
    });
    assert.equal(await tag(f.api), "old2");
  });

  test("refuses an existing tag elsewhere", async () => {
    const f = fakeAPI({
      [`GET repos/${repo}/git/ref/tags/verify/v1.3.0`]: { object: { sha: "old2", type: "commit" } },
      [`GET repos/${repo}/contents/${CHECKSUMS}?ref=old2`]: contents(file({ default: "v1.2.3", versions: { "v1.2.3": sums("a") } })),
    });
    await assert.rejects(tag(f.api), /verify\/v1.3.0 already exists at old2, which isn't stamped for v1.3.0/);
  });
});
