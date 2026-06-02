# Releasing `pkg/lockfile`

`github.com/github/gh-actions-pin/pkg/lockfile` is a Go sub-module
distinct from the parent `github.com/github/gh-actions-pin` module. It
exists so downstream consumers (notably
`github.com/github/actions-workflow-parser`) can import the lockfile
parser without inheriting the CLI's full dependency tree or its
toolchain floor.

## Tag conventions

Sub-module versions live under a path-prefixed tag, per
[Go module documentation][gomod-multi]:

```
pkg/lockfile/v0.1.0
pkg/lockfile/v0.1.1
pkg/lockfile/v1.0.0
```

The parent module continues to use plain semver tags (`v0.x.y`). A
single commit can carry both a parent tag and a `pkg/lockfile/` tag if
both modules need to ship together; tag the parent first, then the
sub-module.

[gomod-multi]: https://go.dev/ref/mod#modules-multiple

## Compatibility floor

`pkg/lockfile/go.mod` pins `go 1.19` deliberately. Downstream consumers
on older toolchains (the workflow parser at the time of writing) must
be able to import this module, so do not bump the directive without
checking with the consumers listed in `pkg/lockfile/CONSUMERS.md` (when
that exists) or in this repo's PR conversation.

The parent module's `go` directive is independent and may be bumped
freely.

## Local development

The parent module references `pkg/lockfile` via a local replace
directive:

```
replace github.com/github/gh-actions-pin/pkg/lockfile => ./pkg/lockfile
```

This means changes inside `pkg/lockfile/` are picked up by the parent
without re-tagging during development. Replace directives are ignored
by downstream consumers, so the published behavior is what matters at
release time.

Run the sub-module tests directly so they don't get skipped by a
parent-rooted `go test ./...`:

```sh
(cd pkg/lockfile && go test ./...)
```
