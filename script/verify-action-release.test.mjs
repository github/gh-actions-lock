import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, test } from "node:test";
import { ACTION, ASSETS, CHECKSUMS, checkTag, publish, stamp } from "./verify-action-release.mjs";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..");

describe("checkTag", () => {
  for (const t of ["v1.2.3", "v0.0.1", "v1.2.3-rc.1"]) test(`accepts ${t}`, () => assert.equal(checkTag(t), t));
  for (const t of ["1.2.3", "v1.2.3;x", "v01.2.3", "v1.2.3-rc.0", "verify-v1.2.3", undefined]) {
    test(`rejects ${t}`, () => assert.throws(() => checkTag(t), /invalid tag/));
  }
});

describe("stamp", () => {
  let root, assetsDir;
  beforeEach(async () => {
    root = await mkdtemp(join(tmpdir(), "stamp-"));
    assetsDir = join(root, "assets");
    await mkdir(join(root, "actions/verify"), { recursive: true });
    await mkdir(assetsDir);
    await writeFile(join(root, ACTION), await readFile(join(repoRoot, ACTION)));
    for (const a of ASSETS) await writeFile(join(assetsDir, a), a);
  });
  afterEach(async () => {
    await rm(root, { recursive: true, force: true });
  });

  test("defaults the version input to the tag and records checksums", async () => {
    await stamp({ root, tag: "v1.2.3-rc.1", assetsDir });
    const action = await readFile(join(root, ACTION), "utf8");
    assert.match(action, /^ {4}default: v1\.2\.3-rc\.1$/m);
    assert.doesNotMatch(action, /stamped by/);
    const lines = (await readFile(join(root, CHECKSUMS), "utf8")).trimEnd().split("\n");
    assert.equal(lines[0], "# v1.2.3-rc.1");
    assert.equal(lines.length, 1 + ASSETS.length);
    const want = createHash("sha256").update("linux-arm64").digest("hex");
    assert.ok(lines.includes(`${want}  linux-arm64`));
  });

  test("rejects an invalid tag", async () => {
    await assert.rejects(stamp({ root, tag: "v1.2.3;x", assetsDir }), /invalid tag/);
  });

  test("fails on a missing release asset", async () => {
    await rm(join(assetsDir, "windows-arm64.exe"));
    await assert.rejects(stamp({ root, tag: "v1.2.3", assetsDir }), /missing release asset/);
  });

  test("refuses to restamp", async () => {
    await stamp({ root, tag: "v1.2.3", assetsDir });
    await assert.rejects(stamp({ root, tag: "v1.2.4", assetsDir }), /exactly one line/);
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
    files: [
      { path: ACTION, content: "a" },
      { path: CHECKSUMS, content: "c" },
    ],
  };

  test("tags a signed commit on top of the release commit", async () => {
    const { calls, fetch } = fakeAPI();
    assert.equal(await publish({ ...args, fetch }), "commit1");
    assert.deepEqual(
      calls.map((c) => `${c.method} ${c.path}`),
      ["POST git/blobs", "POST git/blobs", "POST git/trees", "POST git/commits", "GET git/commits/commit1", "POST git/refs"],
    );
    assert.deepEqual(calls[0].body, { content: Buffer.from("a").toString("base64"), encoding: "base64" });
    assert.deepEqual(calls[2].body, {
      base_tree: "base1",
      tree: [
        { path: ACTION, mode: "100644", type: "blob", sha: "blob1" },
        { path: CHECKSUMS, mode: "100644", type: "blob", sha: "blob2" },
      ],
    });
    assert.deepEqual(calls[3].body.parents, ["head1"]);
    assert.deepEqual(calls[5].body, { ref: "refs/tags/verify-v1.2.3", sha: "commit1" });
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
