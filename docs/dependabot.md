# Dependabot and the Actions lockfile

Dependabot updates action references in workflow YAML and invokes the CLI to
regenerate the corresponding lockfile entries in the same pull request.

> [!NOTE]
> gh-actions-lock is in public preview. Flags, findings JSON, and lockfile
> schema may change between releases, and Dependabot pins a specific CLI version
> and lockfile schema. See [RELEASING.md](../RELEASING.md).

## Lockfile updates

When Dependabot opens a version update for a GitHub Actions dependency in a
workflow that is **already onboarded** to lockfile pinning, it also regenerates
the corresponding lockfile entry, so the pinned commit SHA always matches the
updated ref in your workflow YAML.

The two tools have separate responsibilities:

- **Dependabot owns the workflow YAML.** It decides the new ref, exactly as it
  does today.
- **The `gh-actions-lock` CLI owns the lockfile, exclusively.** Dependabot
  invokes the CLI rather than writing lockfile entries itself, so pinning has a
  single implementation.

If you already use Dependabot for GitHub Actions and you have onboarded
workflows, this needs no configuration.

## Onboarding

Dependabot invokes the CLI with `--no-onboard`, which refuses to add lockfile
entries for workflows or actions that do not already have them. A dependency
Dependabot bumps in a workflow you never locked produces a non-blocking
`onboarding-required` finding and no lockfile write:

```json
{
  "category": "onboarding-required",
  "severity": "info",
  "detail": "actions/checkout@v6 has no lockfile entry; --no-onboard refuses to add new workflows or actions"
}
```

Onboard workflows with the CLI before relying on Dependabot to maintain their
lockfile entries:

```bash
gh actions-lock
```

Dependabot also passes `--no-narrow`, so the CLI does not rewrite the ref
Dependabot just chose. Dependabot picked `v7`; the lockfile records `v7` and its
commit, and the YAML is left alone.

## Reading the update

A Dependabot pull request touching an onboarded workflow changes two files. The
lockfile diff is the security-relevant half:

```diff
 workflows:
     '.github/workflows/ci.yml':
-        - 'actions/checkout@v6.1.0'
+        - 'actions/checkout@v7'
 dependencies:
-    'actions/checkout@v6.1.0':
-        ref: 'v6.1.0'
-        commit: 'sha1-d23441a48e516b6c34aea4fa41551a30e30af803'
+    'actions/checkout@v7':
+        ref: 'v7'
+        commit: 'sha1-3d3c42e5aac5ba805825da76410c181273ba90b1'
         owner_id: 44036562
         repo_id: 197814629
```

Review the changed commit SHA the way you would review any dependency change. A
moved `owner_id` or `repo_id` is a stronger signal still: it means the
repository behind that name is not the one you locked.

Review it, but do not edit it. The lockfile is generated, and the CLI is the
only thing that writes it. If a Dependabot pull request's lockfile looks wrong,
close it or fix the workflow and let the lockfile be regenerated.

## Cooldowns

Dependabot's [`cooldown`
option](https://docs.github.com/en/code-security/reference/supply-chain-security/dependabot-options-reference#cooldown)
delays updates until a release has had time to settle.

```yaml
# .github/dependabot.yml
version: 2
updates:
  - package-ecosystem: "github-actions"
    directory: "/"
    schedule:
      interval: "weekly"
    cooldown:
      default-days: 7
```

`gh actions-lock` reads this file too, so one setting governs both tools.

### What the CLI actually uses it for

Cooldown does **not** hold back ref narrowing. When the CLI narrows
`actions/checkout@v6` to `v6.1.0`, it only considers tags that already point at
the commit `v6` resolves to. Narrowing renames the commit you already have; it
never moves you to a newer release. There is nothing for a cooldown to delay.

What cooldown does control:

- **The fresh-tag nudge.** With no cooldown configured, pinning a tag released
  within the last 3 days emits an informational finding suggesting you configure
  one. Any positive cooldown skips that check for the action.
- **The interactive tag picker**, which hides tags younger than the cooldown
  unless one matches your current pin.

Precedence is Dependabot config first, then
`~/.config/gh-actions-lock/config.yml`. A Dependabot `default-days: 0` does not
count as configured, so it cannot silently downgrade a stricter setting in your
own config file.

### Only `default-days` is honored today

The CLI parses the rest of the cooldown block and reports the keys it ignores:

```
Dependabot cooldown semver-major/minor/patch-days are not supported and were ignored
Dependabot cooldown include/exclude filters are not supported and were ignored
```

These are non-blocking `cooldown-config-ignored` findings. Dependabot still
honors those keys for its own scheduling — the gap is only in what the CLI reads
when deciding whether to nudge you about a fresh tag.

## Automation interaction

If you also run the [lockfile automation
workflow](./repository-developer-experience.md), its `verify` job checks
Dependabot's pull requests like any other. When the lockfile entry is present
and correct, the check passes and nothing else happens.

The `update` job needs more thought. It only runs on `push` to a branch in your
repository, and Dependabot branches live in your repository, so a Dependabot
pull request whose lockfile is stale gets a bot commit fixing it. That means a
Dependabot pull request can gain a second commit. To prevent that, scope the
update job away from Dependabot branches:

```yaml
if: github.event_name == 'push' && github.ref_type == 'branch' && !startsWith(github.ref_name, 'dependabot/')
```

## A note on exit codes

On a fix run the findings JSON reports the state the CLI *diagnosed*, before it
fixed anything. A stale lockfile that the CLI then repaired reports
`"valid": false` with `ref-changed` findings and still exits `0`, because the
run succeeded in fixing it. Re-running reports `"valid": true` with no findings.

Use `--verify` for a read-only answer that writes nothing and exits non-zero if
the lockfile is stale. That is the form to use in a required check.

## Related

- [Setting up lockfile pinning](../README.md)
- [Keeping a repository's Actions lockfile current](./repository-developer-experience.md)
- [Dependabot cooldown options reference](https://docs.github.com/en/code-security/reference/supply-chain-security/dependabot-options-reference#cooldown)
