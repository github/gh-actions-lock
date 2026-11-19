import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, test } from "node:test";
import { ACTION, ASSETS, CHECKSUMS, checkTag, publish, published, stamp, stampedFor } from "./verify-action-release.mjs";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..");

describe("checkTag", () => {
  for (const t of ["v1.2.3", "v0.0.1", "v1.2.3-rc.1"]) test(`accepts ${t}`, () => assert.equal(checkTag(t), t));
  for (const t of ["1.2.3", "v1.2.3;x", "v01.2.3", "v1.2.3-rc.0", "verify-v1.2.3", undefined]) {
    test(`rejects ${t}`, () => assert.throws(() => checkTag(t), /invalid tag/));
  }
});

describe("checked-in stamp", () => {
  test("the version default matches checksums.txt, which covers every asset", async () => {
    const action = await readFile(join(repoRoot, ACTION), "utf8");
    const tag = action.match(/^ {4}default: (\S+) # stamped by/m)?.[1];
    assert.equal(checkTag(tag), tag);
    const lines = (await readFile(join(repoRoot, CHECKSUMS), "utf8")).trimEnd().split("\n");
    assert.equal(lines[0], `# ${tag}`);
    assert.deepEqual(lines.slice(1).map((l) => l.split(/\s+/)[1]), ASSETS);
    for (const l of lines.slice(1)) assert.match(l, /^[0-9a-f]{64}  \S+$/);
  });
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
    assert.match(action, /^ {4}default: v1\.2\.3-rc\.1 # stamped by/m);
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

  test("restamps an earlier stamp and keeps the marker", async () => {
    await stamp({ root, tag: "v1.2.3", assetsDir });
    await stamp({ root, tag: "v1.2.4", assetsDir });
    const action = await readFile(join(root, ACTION), "utf8");
    assert.match(action, /^    default: v1\.2\.4 # stamped by script\/verify-action-release\.mjs$/m);
    assert.match(await readFile(join(root, CHECKSUMS), "utf8"), /^# v1\.2\.4\n/);
  });

  test("stampedFor reports the tag only when the action and checksums agree", async () => {
    assert.equal(await stampedFor(root), null);
    await stamp({ root, tag: "v1.2.3", assetsDir });
    assert.equal(await stampedFor(root), "v1.2.3");
    await writeFile(join(root, CHECKSUMS), "# v1.2.2\n");
    assert.equal(await stampedFor(root), null);
  });

  test("refuses an action without exactly one stamp line", async () => {
    const action = await readFile(join(root, ACTION), "utf8");
    await writeFile(join(root, ACTION), action + action);
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
