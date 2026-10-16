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
- **Disambiguation** from today's positional `<workflow-path>` args (a bare
  `owner/repo` looks like a relative path). Under `--verify`, an argument is a
  remote repo when it is a URL, or it matches `[HOST/]OWNER/REPO[@REF]`, has
  no `.yml`/`.yaml` extension, and does not exist on disk. Anything else is a
  workflow path. Mixing a repo with workflow paths, or giving more than one
  repo, is an error. Without `--verify`, a repo-shaped argument gets a
  targeted error ("remote repositories are only supported with --verify").
  Parsing lives in `internal/repospec` and is shared by `-R` and the positional form.
- Host comes from the repo argument; otherwise `--hostname` / `GH_HOST` /
  default, as today. A host in `-R` that conflicts with `--hostname` is an error.
- Only valid with `--verify`. Rejected in `validateOutputFlags` (run.go) when combined with:
  - no `--verify` (plain runs, `--verify-local`),
  - `--accept-moved`, `--relock`, `--no-migrate-local-actions`,
  - positional `<workflow-path>` args alongside a remote repo (remote mode
    verifies everything the lockfile lists; optional follow-up: treat them as
    repo-relative paths).
- Output (`--json`, findings, exit codes) is unchanged; the JSON schema stays
  additive-only. Add `repo`/`ref` only as new optional fields.

## Design

1. **Repo spec parser** (new `internal/repospec`): parse `OWNER/REPO`,
   `HOST/OWNER/REPO` and URLs into host/owner/repo via `go-gh/repository.Parse`.
   Handle trailing `.git`; a `/tree/<ref>` URL sets the ref when `--ref` is absent.
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

## Tests

- **Go unit** (`internal/**`): repo spec parsing (all forms incl. `@REF`, bad
  input, host conflict); positional disambiguation (existing local path wins,
  `.yml` suffix, URL, mixed args, non-`--verify`); flag validation matrix; remote `FileSource` with httpmock (404
  lockfile, dir listing, SHA pinning); `./`/`$/` resolution against the target repo.
- **Go command test** (`cmd/**`, httpmock, blocking CI): `-R owner/repo --verify`
  happy path, tampered lock entry fails, missing lockfile fails, `--ref`
  honoured, GHE host, nothing written to disk, cwd outside any repo.
- **Catalog scenario**: one `needs_token` scenario against a real fixture repo
  for the live-API delta only; non-blocking, so backed by the Go tests above.

## Docs

README `--verify` section, `--help` text, CHANGELOG entry. Additive, so a minor release.

## Open questions

- Should positional workflow paths narrow the remote scan?
- Accept `/tree/<ref>` / `/blob/` URLs, or only repo-root URLs?
- Dependabot heads-up? Findings JSON and exit codes are unchanged, so likely not needed.
