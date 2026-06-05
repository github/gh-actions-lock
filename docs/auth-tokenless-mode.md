# Auth: tokenless / egress-proxy mode

This doc describes the auth contract the CLI honors so that **hosted
Dependabot** can run `gh-actions-pin` without ever handing it a real
GitHub token. The contract is small but load-bearing — break it and
hosted Dependabot can't ship the CLI engine as the default.

## The contract

The CLI **does not set its own `Authorization` header** on any outbound
HTTP request. All GitHub API auth is delegated to
[`go-gh/v2`](https://github.com/cli/go-gh), which reads `GH_TOKEN` (or
the equivalent host token from `gh auth login` state). Network requests
honor the standard Go `net/http` transport env vars:

- `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` — egress routing.
- `SSL_CERT_FILE`, `SSL_CERT_DIR` — trust roots for the proxy's TLS
  certificate.

This is what lets hosted Dependabot's runtime point us at an
auth-injecting proxy with a dummy `GH_TOKEN`. The proxy strips
whatever placeholder we send and substitutes the real job-scoped
token on its way out. Because we never write our own `Authorization`
header, there is nothing for the proxy's header to collide with.

## Supported runtime modes

| Mode               | `GH_TOKEN`                  | `HTTPS_PROXY`              | Notes                                     |
| ------------------ | --------------------------- | -------------------------- | ----------------------------------------- |
| Developer          | real, from `gh auth login`  | unset                      | Direct egress to `api.github.com`.        |
| CI                 | real, from a secret         | optional                   | Set `HTTPS_PROXY` only if enterprise routes egress through a proxy. |
| Hosted Dependabot  | dummy (e.g. `x-access-token`) | required                 | Proxy injects the real `Authorization` header. May also set `SSL_CERT_FILE` for the proxy CA. |

All three modes share one HTTP client construction path
(`internal/resolver/httpclient.go`). No mode-specific branching, no
mode-specific headers — the env decides everything.

> **Developer-mode footnote — SSO and `GH_TOKEN`.** If you have a
> `GH_TOKEN` env var set to a user PAT and resolution fails on
> `actions/*` with a SAML/SSO error, the cause is that env-var PATs
> are subject to per-org SAML enforcement and the `actions` org
> enforces SSO for user tokens. The keyring-stored OAuth token from
> `gh auth login --web` flows through gh's normal auth path and may
> already be SSO-authorized for that org. Workaround:
> `env -u GH_TOKEN gh actions-pin ...` — unsetting the env var makes
> go-gh fall through to the keyring token. Permanent fix:
> `gh auth login --web` and approve SSO for the `actions` org in the
> browser. This is a dev-machine quirk only; CI and hosted Dependabot
> use app-installation tokens, which are not subject to user-PAT SAML
> enforcement.

## What WILL break this contract

If you are tempted to do any of the following, **don't** — and if you
must, gate it behind an opt-in flag and document it as incompatible
with the Dependabot proxy mode:

- Importing `net/http` and constructing a `*http.Client` with a custom
  `Transport` that sets an `Authorization` header.
- Calling `req.Header.Set("Authorization", ...)` (or the `.Add` /
  map-literal equivalents) anywhere in CLI code.
- Bypassing `go-gh/v2` for any GitHub API call. If go-gh doesn't
  expose what you need, file an upstream issue or extend the resolver
  through its existing `api.ClientOptions` — don't roll your own
  client.
- Hardcoding `https://api.github.com` (or a derived host) in a raw
  HTTP call outside the resolver's client.
- Reading `GH_TOKEN` in CLI code and threading it into a header
  yourself — go-gh already owns that read, and duplicating it
  invites drift.

## How to extend without breaking it

If you need a new HTTP client, route it through
`internal/resolver/httpclient.go`. Reuse `api.ClientOptions` and let
go-gh build the transport so proxy + cert + auth handling stay
consistent. If you genuinely need a non-GitHub HTTP call (e.g. a
metrics endpoint), build a standard `&http.Client{}` with no custom
`Authorization` header — Go's default transport will honor the proxy
env vars on its own.

## Regression guard

`internal/resolver/auth_boundary_test.go` enforces the contract: it
walks all non-test Go source and fails CI if any string literal
`"Authorization"` appears as an HTTP header key. The allowlist is
deliberately narrow — testdata, `_test.go` files, vendored code, and
the boundary test itself. If the test fails, route the offending
client through `internal/resolver/httpclient.go` or, if there is a
genuine reason, extend the allowlist with a comment explaining why.

## See also

- [`dependabot-cli-contract.md`](dependabot-cli-contract.md) §G4 —
  the broader contract this doc unpacks.
- `internal/resolver/httpclient.go` — the one HTTP client construction
  site in CLI code.
