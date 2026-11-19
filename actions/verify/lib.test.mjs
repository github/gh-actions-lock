import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, test } from "node:test";
import { ActionError, assetFor, attestationEnv, checkVersion, downloadFile, install, selectCheck, splitArgs, useEnvProxy, withSubdomainNoProxy } from "./lib.mjs";

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
  const okDownload = (url, dest) => writeFile(dest, binary);
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

  test("names gh when it's missing for an attestation check", async () => {
    const env = { INPUT_VERSION: "v0.4.2", RUNNER_OS: "Linux", RUNNER_ARCH: "X64", RUNNER_TEMP: tmp, PATH: "", Path: "" };
    await assert.rejects(install({ env, actionDir: tmp, download: okDownload, log: quiet }), /gh is not on PATH/);
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
});

describe("downloadFile", () => {
  let tmp;
  beforeEach(async () => {
    tmp = await mkdtemp(join(tmpdir(), "verify-download-"));
  });
  afterEach(async () => {
    await rm(tmp, { recursive: true, force: true });
  });

  const respond = (status, body = "") => new Response(status === 200 ? body : null, { status });

  test("retries transient failures with backoff, then streams the body to disk", async () => {
    const dest = join(tmp, "bin");
    const replies = [() => { throw new TypeError("fetch failed", { cause: new Error("ECONNRESET") }); }, () => respond(503), () => respond(200, "binary")];
    const waits = [];
    await downloadFile("https://example.test/a", dest, {
      fetch: async () => replies.shift()(),
      wait: async (ms) => waits.push(ms),
    });
    assert.equal(await readFile(dest, "utf8"), "binary");
    assert.deepEqual(waits, [1000, 2000]);
  });

  test("does not retry a missing release", async () => {
    let calls = 0;
    await assert.rejects(
      downloadFile("https://example.test/v9.9.9/linux-amd64", join(tmp, "bin"), {
        fetch: async () => (calls++, respond(404)),
        wait: async () => assert.fail("retried"),
      }),
      (err) => err instanceof ActionError && /download https:\/\/example.test\/v9.9.9\/linux-amd64 failed: HTTP 404/.test(err.message),
    );
    assert.equal(calls, 1);
  });

  test("gives up after three retries and reports the cause", async () => {
    let calls = 0;
    await assert.rejects(
      downloadFile("https://example.test/a", join(tmp, "bin"), {
        fetch: async () => { calls++; throw new TypeError("fetch failed", { cause: new Error("getaddrinfo ENOTFOUND") }); },
        wait: async () => {},
      }),
      /failed: getaddrinfo ENOTFOUND/,
    );
    assert.equal(calls, 4);
  });
});

describe("useEnvProxy", () => {
  test("does nothing without a proxy", () => {
    const http = { setGlobalProxyFromEnv: () => assert.fail("set proxy") };
    assert.equal(useEnvProxy({ NO_PROXY: "example.test" }, http), false);
  });
  for (const key of ["HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"]) {
    test(`applies ${key} in process when Node supports it`, () => {
      const env = { [key]: "http://proxy.test:3128" };
      let got;
      assert.equal(useEnvProxy(env, { setGlobalProxyFromEnv: (e) => (got = e) }), false);
      assert.equal(got, env);
    });
  }
  test("asks for a re-run with NODE_USE_ENV_PROXY on older Node", () => {
    assert.equal(useEnvProxy({ https_proxy: "http://proxy.test:3128" }, {}), true);
  });
  test("does not re-run twice", () => {
    assert.equal(useEnvProxy({ https_proxy: "http://proxy.test:3128", NODE_USE_ENV_PROXY: "1" }, {}), false);
  });
});

describe("withSubdomainNoProxy", () => {
  test("adds a subdomain entry for each bare host, in both cases", () => {
    const env = withSubdomainNoProxy({ NO_PROXY: "github.com, .example.test,*.corp,10.0.0.0/8", no_proxy: "githubusercontent.com" });
    assert.equal(env.NO_PROXY, "github.com,.example.test,*.corp,10.0.0.0/8,.github.com");
    assert.equal(env.no_proxy, "githubusercontent.com,.githubusercontent.com");
  });
  test("is idempotent across the proxy re-run", () => {
    const once = withSubdomainNoProxy({ no_proxy: "github.com" });
    assert.deepEqual(withSubdomainNoProxy(once), once);
  });
  test("leaves env without NO_PROXY alone", () => {
    assert.deepEqual(withSubdomainNoProxy({ HTTPS_PROXY: "http://p:1" }), { HTTPS_PROXY: "http://p:1" });
  });
});
