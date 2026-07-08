## Contributing

[fork]: https://github.com/github/gh-actions-lock/fork
[pr]: https://github.com/github/gh-actions-lock/compare
[style]: https://google.github.io/styleguide/go/guide

Hi there! We're thrilled that you'd like to contribute to this project. Your help is essential for keeping it great.

Contributions to this project are [released](https://help.github.com/articles/github-terms-of-service/#6-contributions-under-repository-license) to the public under the [project's open source license](LICENSE).

Please note that this project is released with a [Contributor Code of Conduct](CODE_OF_CONDUCT.md). By participating in this project you agree to abide by its terms.

## Prerequisites for running and testing code

These are one time installations required to be able to test your changes locally as part of the pull request (PR) submission process.

1. Install Go [through download](https://go.dev/doc/install) | [through Homebrew](https://formulae.brew.sh/formula/go). The required version is pinned in [`go.mod`](go.mod).
1. Some integration tests use Ruby; install it [through download](https://www.ruby-lang.org/en/documentation/installation/) | [through Homebrew](https://formulae.brew.sh/formula/ruby) if you plan to run them.

## Submitting a pull request

1. [Fork][fork] and clone the repository.
1. Build the extension: `make build`.
1. Make sure the tests pass on your machine: `make test` (or `go test -race ./...`).
1. Make sure the code is formatted and vetted: `make fmt-check` and `make vet`.
1. Create a new branch: `git checkout -b my-branch-name`.
1. Make your change, add tests, and make sure the tests still pass.
1. Push to your fork and [submit a pull request][pr].
1. Pat yourself on the back and wait for your pull request to be reviewed and merged.

Here are a few things you can do that will increase the likelihood of your pull request being accepted:

- Follow the [Go style guide][style] and match the conventions in [`AGENTS.md`](AGENTS.md).
- Write tests.
- Keep your change as focused as possible. If there are multiple changes you would like to make that are not dependent upon each other, consider submitting them as separate pull requests.
- Write a [good commit message](http://tbaggery.com/2008/04/19/a-note-about-git-commit-messages.html).

## Resources

- [How to Contribute to Open Source](https://opensource.guide/how-to-contribute/)
- [Using Pull Requests](https://help.github.com/articles/about-pull-requests/)
- [GitHub Help](https://help.github.com)
