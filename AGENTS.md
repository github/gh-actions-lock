# Agent instructions

## Code style

Write Go that follows the [Google Go Style Guide](https://google.github.io/styleguide/go/guide) and lines up with [cli/cli](https://github.com/cli/cli) conventions. Keep it readable and simple.

### Comments

Doc comments earn their keep by saying something the code doesn't. Skip:

- Restating the signature ("Foo does foo").
- Meta-commentary about what the package _isn't_ ("intentionally pure", "no HTTP, no filesystem"). If it's true, the imports show it.
- Stability sermons on every type. One short note in the package doc is enough.
- Decorative em-dashes, hedging, or apologetic phrasing.

Keep:

- Why a non-obvious choice was made.
- Invariants the type or function relies on.
- Pointers to related code when the link isn't obvious from names.

When in doubt, fewer words. If a comment reads like marketing or a victory lap, delete it.

### Naming and shape

- Prefer short, lowercase package names. No underscores.
- Exported identifiers get a doc comment that starts with the identifier name.
- Errors are values; wrap with `fmt.Errorf("...: %w", err)` and expose sentinels with `errors.Is`.
- Don't pre-declare `var foo Type` then assign — declare at first use.

### Tests

- Table-driven by default; subtests named with what they assert, not how.
- One assertion per logical claim; multiple `assert.X` calls beat one giant struct compare.
- Don't golden-snapshot what you can assert structurally.
