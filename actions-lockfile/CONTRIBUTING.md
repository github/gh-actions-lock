# Contributing

`actions-lockfile` defines the parser and schema for the GitHub Actions
dependency lockfile format. Bug reports, doc fixes, and small parser
improvements are welcome. Schema changes need coordination — see below.

## Where to file issues

- **During staging** (this directory lives in `github/gh-actions-pin`):
  open issues at [`github/gh-actions-pin`](https://github.com/github/gh-actions-pin/issues)
  and prefix the title with `actions-lockfile:`.
- **After promotion** to `github.com/github/actions-lockfile`: file issues
  in that repository.

## Dev loop

The package has no network dependencies; it is unit-test only.

```sh
go test ./...
go vet ./...
gofmt -l .
```

During staging, the Go source lives at `pkg/lockfile/` in `gh-actions-pin`,
so run those commands from there:

```sh
cd pkg/lockfile && go test ./... && go vet ./... && gofmt -l .
```

## Schema-change policy

The lockfile schema is the contract between `gh actions-pin` (the producer)
and every consumer that audits or verifies pins. We coordinate changes
deliberately:

- **Backward-compatible additions** (new optional fields, new top-level keys
  consumers can ignore) can ship in a minor schema version. Bump the
  `Version` const, the embedded JSON Schema's `$id`, and document the change
  in `RELEASING.md`.
- **Breaking changes** (renames, removals, type changes) require a new
  schema major version and a coordinated migration in `gh-actions-pin`. Open
  an issue first; do not open a PR with a breaking schema change cold.
- **Pin grammar changes** (the `OWNER/REPO@REF:ALGO-HEX` shape) are always
  breaking; same rules.

## Public-API stability

- Pre-1.0, the package may remove incidentally-exported helpers without a
  major bump. The README's "Compatibility and stability" section names the
  current candidates.
- New exports require a justification in the PR. The package's public
  surface is intentionally narrow.

## Style

- `gofmt` is the formatter. CI checks `gofmt -l .` is empty.
- Tests live next to the code (`foo.go` → `foo_test.go`). Use `testify`'s
  `assert` and `require` — already a dependency.
- Doc comments on every exported symbol. `go vet` and `go doc` are the
  acceptance bar.
