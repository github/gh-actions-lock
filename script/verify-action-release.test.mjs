import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, test } from "node:test";
import { parseChecksums } from "../actions/verify/lib.mjs";
import { ASSETS, CHECKSUMS, checkTag, publish, published, stamp, stampedFor } from "./verify-action-release.mjs";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..");

describe("checkTag", () => {
  for (const t of ["v1.2.3", "v0.0.1", "v1.2.3-rc.1"]) test(`accepts ${t}`, () => assert.equal(checkTag(t), t));
  for (const t of ["1.2.3", "v1.2.3;x", "v01.2.3", "v1.2.3-rc.0", "verify-v1.2.3", undefined]) {
    test(`rejects ${t}`, () => assert.throws(() => checkTag(t), /invalid tag/));
  }
});

describe("checked-in stamp", () => {
  test("checksums.json covers every release asset, and action.yml has no version default", async () => {
    const sums = parseChecksums(await readFile(join(repoRoot, CHECKSUMS), "utf8"));
    assert.equal(checkTag(sums.version), sums.version);
    assert.deepEqual(Object.keys(sums.sha256), ASSETS);
    const action = await readFile(join(repoRoot, "actions/verify/action.yml"), "utf8");
    assert.doesNotMatch(action.slice(action.indexOf("  version:"), action.indexOf("  args:")), /default:/);
  });
});

describe("stamp", () => {
  let root, assetsDir;
  beforeEach(async () => {
    root = await mkdtemp(join(tmpdir(), "stamp-"));
    assetsDir = join(root, "assets");
    await mkdir(join(root, "actions/verify"), { recursive: true });
    await mkdir(assetsDir);
    for (const a of ASSETS) await writeFile(join(assetsDir, a), a);
  });
  afterEach(async () => {
    await rm(root, { recursive: true, force: true });
  });

  test("records the tag and every asset's checksum in a form the action reads", async () => {
    await stamp({ root, tag: "v1.2.3-rc.1", assetsDir });
    const sums = parseChecksums(await readFile(join(root, CHECKSUMS), "utf8"));
    assert.equal(sums.version, "v1.2.3-rc.1");
    assert.deepEqual(Object.keys(sums.sha256), ASSETS);
    assert.equal(sums.sha256["linux-arm64"], createHash("sha256").update("linux-arm64").digest("hex"));
  });

  test("rejects an invalid tag", async () => {
    await assert.rejects(stamp({ root, tag: "v1.2.3;x", assetsDir }), /invalid tag/);
  });

  test("fails on a missing release asset", async () => {
    await rm(join(assetsDir, "windows-arm64.exe"));
    await assert.rejects(stamp({ root, tag: "v1.2.3", assetsDir }), /missing release asset/);
  });

  test("restamps an earlier stamp", async () => {
    await stamp({ root, tag: "v1.2.3", assetsDir });
    await stamp({ root, tag: "v1.2.4", assetsDir });
    assert.equal(await stampedFor(root), "v1.2.4");
  });

  test("stampedFor is null when checksums.json is missing or malformed", async () => {
    assert.equal(await stampedFor(root), null);
    await writeFile(join(root, CHECKSUMS), "version: v1.2.2\n");
    assert.equal(await stampedFor(root), null);
  });
});

describe("publish", () => {
  function fakeAPI({ verified = true } = {}) {
    const calls = [];
    const fetch = async (url, { method, body, headers }) => {
      assert.equal(headers.authorization, "Bearer tok");
      const path = url.replace("https://api.test/repos/o/r/", "");
      calls.push({ method, path, body: body && JSON.parse(body) });
      const res = {
        "git/blobs": { sha: `blob${calls.length}` },
        "git/trees": { sha: "tree1" },
        "git/commits": { sha: "commit1" },
        "git/commits/commit1": { verification: { verified } },
        "git/refs": {},
      }[path];
      return new Response(JSON.stringify(res), { status: 201 });
    };
    return { calls, fetch };
  }
  const args = {
    api: "https://api.test",
    token: "tok",
    repo: "o/r",
    tag: "v1.2.3",
    head: "head1",
    baseTree: "base1",
    files: [{ path: CHECKSUMS, content: "c" }],
  };

  test("tags a signed commit on top of the release commit", async () => {
    const { calls, fetch } = fakeAPI();
    assert.equal(await publish({ ...args, fetch }), "commit1");
    assert.deepEqual(
      calls.map((c) => `${c.method} ${c.path}`),
      ["POST git/blobs", "POST git/trees", "POST git/commits", "GET git/commits/commit1", "POST git/refs"],
    );
    assert.deepEqual(calls[0].body, { content: Buffer.from("c").toString("base64"), encoding: "base64" });
    assert.deepEqual(calls[1].body, {
      base_tree: "base1",
      tree: [{ path: CHECKSUMS, mode: "100644", type: "blob", sha: "blob1" }],
    });
    assert.deepEqual(calls[2].body.parents, ["head1"]);
    assert.deepEqual(calls[4].body, { ref: "refs/tags/verify-v1.2.3", sha: "commit1" });
  });

  test("does not tag an unsigned commit", async () => {
    const { calls, fetch } = fakeAPI({ verified: false });
    await assert.rejects(publish({ ...args, fetch }), /not signed/);
    assert.ok(!calls.some((c) => c.path === "git/refs"));
  });

  test("surfaces API errors", async () => {
    const fetch = async () => new Response("Reference already exists", { status: 422 });
    await assert.rejects(publish({ ...args, fetch }), /POST git\/blobs: HTTP 422 Reference already exists/);
  });
});

describe("published", () => {
  const args = { api: "https://api.test", token: "tok", repo: "o/r", tag: "v1.2.3", head: "head1", tree: "tree1" };
  function fakeAPI(responses) {
    return async (url) => {
      const res = responses[url.replace("https://api.test/repos/o/r/", "")];
      return res ? new Response(JSON.stringify(res), { status: 200 }) : new Response("Not Found", { status: 404 });
    };
  }
  const ref = { "git/ref/tags/verify-v1.2.3": { object: { type: "commit", sha: "stamp1" } } };

  test("returns null when the tag doesn't exist", async () => {
    assert.equal(await published({ ...args, fetch: fakeAPI({}) }), null);
  });

  test("returns a signed stamp commit on head", async () => {
    const fetch = fakeAPI({ ...ref, "git/commits/stamp1": { parents: [{ sha: "head1" }], tree: { sha: "tree1" }, verification: { verified: true } } });
    assert.equal(await published({ ...args, fetch }), "stamp1");
  });

  for (const [name, commit] of [
    ["another parent", { parents: [{ sha: "other" }], tree: { sha: "tree1" }, verification: { verified: true } }],
    ["another tree", { parents: [{ sha: "head1" }], tree: { sha: "tree2" }, verification: { verified: true } }],
    ["an unsigned commit", { parents: [{ sha: "head1" }], tree: { sha: "tree1" }, verification: { verified: false } }],
    ["a merge commit", { parents: [{ sha: "head1" }, { sha: "x" }], tree: { sha: "tree1" }, verification: { verified: true } }],
  ]) {
    test(`fails on ${name}`, async () => {
      const fetch = fakeAPI({ ...ref, "git/commits/stamp1": commit });
      await assert.rejects(published({ ...args, fetch }), /verify-v1\.2\.3 already exists at stamp1/);
    });
  }

  test("fails on an annotated tag object", async () => {
    const fetch = fakeAPI({ "git/ref/tags/verify-v1.2.3": { object: { type: "tag", sha: "tagobj" } } });
    await assert.rejects(published({ ...args, fetch }), /already exists at tagobj/);
  });
});
