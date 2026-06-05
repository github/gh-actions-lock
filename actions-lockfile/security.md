# Security

## Reporting a vulnerability

**During staging** (this directory lives in `github/gh-actions-pin`): use the
[`github/gh-actions-pin` security advisory process](https://github.com/github/gh-actions-pin/security/advisories/new).
Do not file public issues for security reports.

**After promotion** to `github.com/github/actions-lockfile`: use that
repository's security advisory process.

For general guidance on coordinated disclosure with GitHub, see
[GitHub's security policy](https://github.com/github/.github/blob/main/SECURITY.md).

## Threat model (informational)

`actions-lockfile` is a parser. It reads a YAML document that conforms to a
known schema and exposes structured data and positions to callers. Concrete
risks worth flagging in a report:

- Maliciously crafted lockfile contents that cause the parser to panic,
  exhaust memory, or hang.
- Schema-validation bypasses where invalid input is accepted as valid.
- Position-tracking bugs that cause downstream tools to point users at the
  wrong line/column when surfacing diagnostics.

Out of scope for reports against this package:

- Whether a particular pin is policy-compliant. That is the consumer's
  decision.
- Whether `gh actions-pin` resolves the right SHA. Report those against the
  CLI.
- Whether a workflow YAML file is well-formed. Report those against
  [`actions/languageservices/workflow-parser`](https://github.com/actions/languageservices).
