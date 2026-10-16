# Plan: `--verify` against a remote repository (#164)

## Goal

`gh actions-lock --verify -R <repo>` verifies a repository's `actions.lock`
and every workflow it covers without cloning or writing anything. Reusable
workflows and composite actions are followed remotely through the API.

## CLI surface (cli/cli conventions)

- `-R, --repo [HOST/]OWNER/REPO`, the same flag and syntax as `gh`. Also accepts
  `https://github.com/OWNER/REPO` and `https://HOST/OWNER/REPO`.
- `--ref <branch|tag|sha>`: ref to read. Defaults to the repo's default branch.
  `gh` has no `owner/repo@ref` form for `-R`, so we don't add one.
- **Positional form**, like `gh repo clone`'s repository argument:
  `gh actions-lock --verify OWNER/REPO[@REF]`, `HOST/OWNER/REPO[@REF]`, or a
  full URL (`https://github.com/OWNER/REPO`, `https://HOST/OWNER/REPO`).
  `@REF` is an extension beyond `gh repo clone` (which has no ref syntax) and
  is equivalent to `--ref`. Specifying both `@REF` and `--ref`, or both a
  positional repo and `-R`, is an error unless the values are identical.
- **Single workflow(s)**: `gh actions-lock --verify OWNER/REPO[@REF] .github/workflows/ci.yml`
  verifies only the listed workflows. Paths are relative to the repo root and
  must exist at the pinned SHA (a missing path is an error, not a skip). The
  whole `actions.lock` is still fetched and parsed; narrowing only limits which
  workflows are checked, and their transitive dependencies are followed as usual.
  With `-R OWNER/REPO`, all positional args are repo-relative paths.
- **Disambiguation** from today's positional `<workflow-path>` args (a bare
  `owner/repo` looks like a relative path). Under `--verify` without `-R`,
  only the first positional can be a repo: it is one when it is a URL, or
  matches `[HOST/]OWNER/REPO[@REF]`, has no `.yml`/`.yaml` extension, and does
  not exist on disk. Every later argument is a repo-relative workflow path, so
  the on-disk check never applies to them. If the first argument is not a repo,
  all arguments are local workflow paths, as today. Without `--verify`,
  positional args are always workflow paths and repo detection never runs, so
  the remote form can't leak into other modes.
  Parsing lives in `internal/repospec` and is shared by `-R` and the positional form.
- Host comes from the repo argument; otherwise `--hostname` / `GH_HOST` /
  default, as today. A host in `-R` that conflicts with `--hostname` is an error.
- Only valid with `--verify`. Rejected in `validateOutputFlags` (run.go) when combined with:
  - no `--verify` (plain runs, `--verify-local`),
  - `--accept-moved`, `--relock`, `--no-migrate-local-actions`,
  - a second repo-shaped positional or both `-R` and a positional repo with differing values.
- Output (`--json`, findings, exit codes) is unchanged; the JSON schema stays
  additive-only. Add `repo`/`ref` only as new optional fields.

## Design

1. **Repo spec parser** (new `internal/repospec`): parse `OWNER/REPO`,
   `HOST/OWNER/REPO` and URLs into host/owner/repo via `go-gh/repository.Parse`.
   Handle trailing `.git`; a `/tree/<ref>`, `/releases/tag/<tag>` or
   `/commit/<sha>` URL sets the ref when `--ref` is absent.
2. **Ref resolution**: resolve `--ref` (or default branch) to a commit SHA once
   via `Client.CommitSHA` / `GetDefaultBranch`. Pin every later read to that
   SHA so verification is a consistent snapshot, and report the SHA.
3. **Remote content source**: add a `FileSource` interface
   (`ReadFile(ctx, path)`, `ListDir(ctx, path)`) with a local-filesystem
   implementation (current behavior) and a remote one pinned to the SHA.
   Reuse the existing fetch pattern in `internal/ghapi/graphql_action_files.go`:
   - `Client.ResolveActionFiles` already takes `ActionFileRequest{Owner, Repo, Path, Ref}`
     and returns the commit OID plus `action.yml`/`action.yaml` text in one
     batched GraphQL query. It splits and retries failed batches, falls back to
     REST (`retryWithAnonymous`, `resolveAnonymous`, `anonGetFileContent`), and
     handles renames and SSO. Target-repo `./…` and `$/…` composites become
     `ActionFileRequest{target owner, repo, path, ref=pinned SHA}` with no new fetcher.
   - Gap: that query reads only `action.yml`/`action.yaml`. Add a sibling
     generic reader, `ReadFiles(ctx, []FileRequest{Owner, Repo, Path, Ref})`,
     using `object(expression: "<sha>:<path>")`, batched, split-on-failure and
     REST-fallback in the same way (share the batching/split helpers). It
     fetches `actions.lock`, workflow files and reusable-workflow files.
   - Gap: directory listing of `.github/workflows/` via a tree/`object` entries
     query (REST contents fallback).
4. **Lockfile**: fetch `.github/workflows/actions.lock` from the source and
   parse from bytes (add `lockfile.Parse([]byte)` if missing; today
   `LoadStateAt` reads a path). A missing lockfile is a blocking finding, not a crash.
5. **Workflow discovery**: list `.github/workflows/*.y{,a}ml` remotely and feed
   contents into `pipeline.ParseAll`; `workflowfile` currently takes file
   paths, so refactor it to accept bytes.
6. **Following dependencies remotely**: `./…` and `$/…` refs resolve against
   the target repo at the pinned SHA via `FileSource`, not the cwd. External
   actions and reusable workflows already resolve through the API; the
   existing transitive traversal is reused and nothing touches disk.
7. **Checks**: run the existing `--verify` pipeline (no-fix) over the remote
   source. Runner-parity and reachability are already API-based.
   Self-repository classification (`workflowfile/selfrepository.go`) must use
   the target repo, not the cwd repo.
8. **Safety**: strictly read-only. No run log or cache written into the cwd
   repo, no migration, no prompts (force no-interactive). Normal token for the
   target host; unauthenticated only for public repos.
9. **Renamed or transferred target**: the target repo is the trust root. The
   lockfile does not record the target's own `owner_id`/`repo_id`, so a repo
   *replaced* at the same name cannot be detected and is not claimed.
   - Follow the redirect and verify normally; reads stay pinned to the SHA.
   - If the API's canonical `full_name` differs from the request by more than
     case, add a non-blocking `repo-renamed` finding naming the new location
     (additive; verification result unchanged). Case-only differences are silent.
   - A 404 or inaccessible target is an error, never a finding.
   - Dependencies of the target keep today's rename/replace/unavailable
     classification (`pipeline/parity.go`), which uses recorded IDs.
   - `./…`/`$/…` composites are read from the same repo and SHA, so a rename
     cannot affect them. Reusable workflows are locked dependencies and get the
     dependency classification above.
   - `ghapi` contents reads must expose the canonical name so this is observable.
   - Pin exactly with a `/commit/<sha>` URL or `--ref <sha>`; unaffected by renames.
10. **Reuse first**: before adding anything, check gh-actions-lock and gh-cli/go-gh.
    Repo specs use `repository.ParseWithHost`; the host is the spec host, then
    `--hostname`, then `auth.DefaultHost()`. Unlike local runs it never consults
    the cwd repo or `GH_HOST` discovery in `resolveHostname`.
11. **Structure**: `--verify` dispatches through a `verifier` interface
    (`selectVerifier`) with `localVerifier` and `remoteVerifier`; `pipeline`
    reads workflows through a `workflowReader` (`localReader`, `sourceReader`)
    and `workflowfile` shares one scan core. Remote verify never calls
    `repository.Current()`, the cooldown lookup, local composite extraction or
    the run log.
12. **Coverage rule**: a remote repo with no lockfile fails like a local one
    (every workflow is `not-pinned`); there is no separate "require lockfile" flag.
13. **Write-mode flags** (`--accept-moved`, `--relock`, ...) are rejected with
    `-R`; `--accept-moved` has nothing to accept for the target.
14. **Exclusive target forms**: `-R`/`--repo` or a positional `OWNER/REPO[@REF]`,
    never both; they are not merged. Any http(s) URL positional is treated as a
    repo and surfaces the URL parse error.
15. **Host pinning**: the target is pinned to its host before the metadata
    lookup, so a `*.ghe.com` 404 never falls back to a github.com namesake.
16. **Missing lockfile** adds an explicit blocking `not-pinned` repository
    finding, so workflows with only local actions cannot pass.
17. **Explicit workflow paths** expand through same-repo reusable workflow calls
    (`./.github/workflows/x.yml`, `$/...`), deduplicated for cycles; a missing
    callee is an error.
18. `action.yaml` is read only when `action.yml` is absent; other errors propagate.
19. Contents reads use the SAML/SSO anonymous fallback like other API calls.

## Tests

- **Go unit** (`internal/**`): repo spec parsing (all forms incl. `@REF`, bad
  input, host conflict); narrowing to listed workflows (missing path errors,
  lockfile still fully loaded); positional disambiguation (existing local path wins,
  only the first arg can be a repo,
  `.yml` suffix, URL, mixed args, non-`--verify` args stay workflow paths); flag validation matrix; remote `FileSource` with httpmock (404
  lockfile, dir listing, SHA pinning); `./`/`$/` resolution against the target repo.
- **Go command test** (`cmd/**`, httpmock, blocking CI): `-R owner/repo --verify`
  happy path, tampered lock entry fails, missing lockfile fails, `--ref`
  honoured, GHE host, nothing written to disk, cwd outside any repo; renamed
  and transferred target yields a non-blocking `repo-renamed` finding while a
  case-only difference yields none; 404 target errors.
- **Catalog scenario**: one `needs_token` scenario against a real fixture repo
  for the live-API delta only; non-blocking, so backed by the Go tests above.

## Docs

README `--verify` section, `--help` text, CHANGELOG entry. Additive, so a minor release.

## Open questions

- Accept `/blob/` URLs? (Currently rejected; `/tree/`, `/releases/tag/` and
  `/commit/` URLs are accepted.)
- Dependabot heads-up? Findings JSON and exit codes are unchanged, so likely not needed.
