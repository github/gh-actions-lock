import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, test } from "node:test";
import { ActionError, assetFor, attestationEnv, checkVersion, install, selectCheck, splitArgs } from "./lib.mjs";

const sha = (s) => createHash("sha256").update(s).digest("hex");

describe("splitArgs", () => {
  test("splits on spaces, tabs and newlines", () => {
    assert.deepEqual(splitArgs("--verify\n--fail-on-warning\t --json\r\n"), ["--verify", "--fail-on-warning", "--json"]);
  });
  test("passes glob characters through unexpanded", () => {
    assert.deepEqual(splitArgs("--verify *"), ["--verify", "*"]);
  });
  test("empty input gives no args", () => {
    assert.deepEqual(splitArgs(""), []);
    assert.deepEqual(splitArgs(undefined), []);
  });
});

describe("checkVersion", () => {
  test("accepts releases and release candidates", () => {
    assert.equal(checkVersion("v0.4.1"), "v0.4.1");
    assert.equal(checkVersion("v1.2.3-rc.4"), "v1.2.3-rc.4");
  });
  test("asks for a version when empty", () => {
    assert.throws(() => checkVersion(""), /set the version input/);
  });
  for (const v of ["0.4.1", "v0.4", "v0.4.1;rm", "../v0.4.1", "v0.4.1-beta"]) {
    test(`rejects ${v}`, () => assert.throws(() => checkVersion(v), /invalid version/));
  }
});

describe("assetFor", () => {
  test("maps runners to release assets", () => {
    assert.deepEqual(assetFor("Linux", "X64"), { asset: "linux-amd64", ext: "" });
    assert.deepEqual(assetFor("macOS", "ARM64"), { asset: "darwin-arm64", ext: "" });
    assert.deepEqual(assetFor("Windows", "X64"), { asset: "windows-amd64.exe", ext: ".exe" });
  });
  test("rejects unsupported runners", () => {
    assert.throws(() => assetFor("FreeBSD", "X64"), /unsupported runner OS/);
    assert.throws(() => assetFor("Linux", "X86"), /unsupported runner architecture/);
  });
});

describe("selectCheck", () => {
  const sums = `# v0.4.1\n${"a".repeat(64)}  linux-amd64\n${"b".repeat(64)}  windows-amd64.exe\n`;
  test("uses the stamped checksum for the stamped version", () => {
    assert.deepEqual(selectCheck("v0.4.1", sums, "windows-amd64.exe"), { kind: "checksum", want: "b".repeat(64) });
  });
  test("handles CRLF checksums", () => {
    assert.deepEqual(selectCheck("v0.4.1", sums.replaceAll("\n", "\r\n"), "linux-amd64"), {
      kind: "checksum",
      want: "a".repeat(64),
    });
  });
  test("uses the attestation for any other version", () => {
    assert.deepEqual(selectCheck("v0.4.2", sums, "linux-amd64"), { kind: "attestation" });
  });
  test("uses the attestation when the action is unstamped", () => {
    assert.deepEqual(selectCheck("v0.4.1", null, "linux-amd64"), { kind: "attestation" });
  });
  test("fails when the stamped version lacks the asset", () => {
    assert.throws(() => selectCheck("v0.4.1", sums, "darwin-arm64"), /no checksum for darwin-arm64/);
  });
});

describe("attestationEnv", () => {
  const base = { INPUT_TOKEN: "job", INPUT_VERSION: "v0.4.2", GITHUB_TOKEN: "ambient", PATH: "/bin" };
  test("on github.com uses the token input and drops GITHUB_TOKEN", () => {
    const env = attestationEnv({ ...base, GITHUB_SERVER_URL: "https://github.com" });
    assert.equal(env.GH_TOKEN, "job");
    assert.equal(env.GH_HOST, "github.com");
    assert.equal(env.PATH, "/bin");
    assert.ok(!("GITHUB_TOKEN" in env));
  });
  test("off github.com uses GH_ACTIONS_LOCK_GITHUB_COM_TOKEN, never the tenant token", () => {
    const env = attestationEnv({
      ...base,
      GITHUB_SERVER_URL: "https://tenant.ghe.com",
      GH_ACTIONS_LOCK_GITHUB_COM_TOKEN: "dotcom",
    });
    assert.equal(env.GH_TOKEN, "dotcom");
    assert.equal(env.GH_HOST, "github.com");
    assert.ok(!("GITHUB_TOKEN" in env));
  });
  test("off github.com fails without a github.com token", () => {
    assert.throws(
      () => attestationEnv({ ...base, GITHUB_SERVER_URL: "https://tenant.ghe.com" }),
      /set GH_ACTIONS_LOCK_GITHUB_COM_TOKEN to check v0.4.2/,
    );
  });
});

describe("install", () => {
  let tmp;
  beforeEach(async () => {
    tmp = await mkdtemp(join(tmpdir(), "verify-action-"));
  });
  afterEach(async () => {
    await rm(tmp, { recursive: true, force: true });
  });

  const binary = "fake gh-actions-lock";
  const okDownload = async (url, dest) => (await writeFile(dest, binary), true);
  const quiet = () => {};

  async function stampedAction(version, asset, sum = sha(binary)) {
    await writeFile(join(tmp, "checksums.txt"), `# ${version}\n${sum}  ${asset}\n`);
    return tmp;
  }

  test("checks the stamped checksum, with backslashes in the path", async () => {
    // GNU sha256sum escaped backslash paths; hashing in-process must not care.
    const env = {
      INPUT_VERSION: "v0.4.1",
      RUNNER_OS: "Windows",
      RUNNER_ARCH: "X64",
      RUNNER_TEMP: join(tmp, "runner\\temp"),
    };
    const actionDir = await stampedAction("v0.4.1", "windows-amd64.exe");
    let fetched;
    const bin = await install({
      env,
      actionDir,
      download: async (url, dest) => ((fetched = url), okDownload(url, dest)),
      gh: () => assert.fail("attestation should not run"),
      log: quiet,
    });
    assert.equal(fetched, "https://github.com/github/gh-actions-lock/releases/download/v0.4.1/windows-amd64.exe");
    assert.equal(bin, join(env.RUNNER_TEMP, "gh-actions-lock", "gh-actions-lock.exe"));
    assert.equal(await readFile(bin, "utf8"), binary);
  });

  test("fails on a checksum mismatch", async () => {
    const env = { INPUT_VERSION: "v0.4.1", RUNNER_OS: "Linux", RUNNER_ARCH: "X64", RUNNER_TEMP: tmp };
    const actionDir = await stampedAction("v0.4.1", "linux-amd64", "0".repeat(64));
    await assert.rejects(
      install({ env, actionDir, download: okDownload, gh: () => true, log: quiet }),
      (err) => err instanceof ActionError && /checksum mismatch for linux-amd64 v0.4.1/.test(err.message),
    );
  });

  test("checks other versions against the attestation with a scoped token", async () => {
    const env = {
      INPUT_VERSION: "v0.4.2",
      INPUT_TOKEN: "tenant",
      GITHUB_TOKEN: "ambient",
      GITHUB_SERVER_URL: "https://tenant.ghe.com",
      GH_ACTIONS_LOCK_GITHUB_COM_TOKEN: "dotcom",
      RUNNER_OS: "macOS",
      RUNNER_ARCH: "ARM64",
      RUNNER_TEMP: tmp,
    };
    const actionDir = await stampedAction("v0.4.1", "darwin-arm64");
    let call;
    const bin = await install({ env, actionDir, download: okDownload, gh: (args, e) => ((call = { args, e }), true), log: quiet });
    assert.deepEqual(call.args, [
      "attestation", "verify", bin,
      "--repo", "github/gh-actions-lock",
      "--signer-workflow", "github/gh-actions-lock/.github/workflows/release.yml",
      "--source-ref", "refs/tags/v0.4.2",
    ]);
    assert.equal(call.e.GH_TOKEN, "dotcom");
    assert.ok(!("GITHUB_TOKEN" in call.e));
  });

  test("fails when the attestation check fails", async () => {
    const env = { INPUT_VERSION: "v0.4.2", RUNNER_OS: "Linux", RUNNER_ARCH: "X64", RUNNER_TEMP: tmp };
    await assert.rejects(
      install({ env, actionDir: tmp, download: okDownload, gh: () => false, log: quiet }),
      /attestation check failed for linux-amd64 v0.4.2/,
    );
  });

  test("checks the token scope before downloading", async () => {
    const env = {
      INPUT_VERSION: "v0.4.2",
      GITHUB_SERVER_URL: "https://tenant.ghe.com",
      RUNNER_OS: "Linux",
      RUNNER_ARCH: "X64",
      RUNNER_TEMP: tmp,
    };
    await assert.rejects(
      install({ env, actionDir: tmp, download: () => assert.fail("downloaded"), gh: () => true, log: quiet }),
      /GH_ACTIONS_LOCK_GITHUB_COM_TOKEN/,
    );
  });

  test("fails when the download fails", async () => {
    const env = { INPUT_VERSION: "v0.4.1", RUNNER_OS: "Linux", RUNNER_ARCH: "X64", RUNNER_TEMP: tmp };
    const actionDir = await stampedAction("v0.4.1", "linux-amd64");
    await assert.rejects(
      install({ env, actionDir, download: () => false, gh: () => true, log: quiet }),
      /download https:\/\/github.com\/github\/gh-actions-lock\/releases\/download\/v0.4.1\/linux-amd64 failed/,
    );
  });
});
