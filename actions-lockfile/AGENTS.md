# AGENTS.md

Guidance for automated agents and contributors working in this package.

## Layout

- `pkg/lockfile/` (during staging) — the Go source. After promotion, the
  package lives at the repository root.
- `pkg/lockfile/lockfile-v0.0.1.json` — the JSON Schema 2020-12 document
  describing the lockfile format. Embedded into the package via `//go:embed`.
- `pkg/lockfile/testdata/` — `action.yml` fixtures for `ParseActionMeta`.

## Test command

```sh
cd pkg/lockfile && go test ./...
```

The package has no network dependencies. Do not introduce any.

## Public-API stability

The intended stable exported surface is documented in the README's
"Compatibility and stability" section. Do not expand the public API surface
without an issue first; pre-1.0 the package is actively trying to shrink.

## Schema changes

Any change to `lockfile-v*.json`, the YAML field tags on `File` /
`Action`, or the pin grammar requires coordination with `github/gh-actions-pin`
(the producer) and a CONTRIBUTING.md-compliant version bump. Do not modify
the schema without an explicit issue describing the migration.

## Out of scope

- Resolving action pins, fetching from `api.github.com`, or making policy
  decisions about which pins are acceptable. That is the CLI's job.
- Parsing workflow `.yml` files beyond the `uses:` field. That is
  [`actions/languageservices/workflow-parser`](https://github.com/actions/languageservices)'s
  job.
