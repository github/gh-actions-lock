## Contributing

[fork]: https://github.com/github/gh-actions-lock/fork
[pr]: https://github.com/github/gh-actions-lock/compare
[style]: https://google.github.io/styleguide/go/guide

Hi there! We're thrilled that you'd like to contribute to this project. Your help is essential for keeping it great.

Contributions to this project are [released](https://help.github.com/articles/github-terms-of-service/#6-contributions-under-repository-license) to the public under the [project's open source license](LICENSE).

Please note that this project is released with a [Contributor Code of Conduct](CODE_OF_CONDUCT.md). By participating in this project you agree to abide by its terms.

## Prerequisites for running and testing code

Install the tools for the checks you plan to run:

- **Go**: required for builds and Go tests. Install it [from go.dev](https://go.dev/doc/install); the minimum version is specified in [`go.mod`](go.mod).
- **Git**: required by the integration test harness.
- **GNU Make**: required for the `make` targets below. CMake is a different tool and does not provide `make`.
- **Bash and a C compiler**: required by `make test` on Linux. That target runs the Bash `script/release-test` and Go's race-enabled tests (which need cgo and a C toolchain).
- **Ruby, WEBrick, and jq**: required for integration tests. Install Ruby [from ruby-lang.org](https://www.ruby-lang.org/en/documentation/installation/), then install WEBrick with `gem install webrick`. The integration scenarios use `jq` for JSON assertions.
- **GitHub CLI and a token**: needed only for live integration scenarios; stub scenarios do not require authentication.

On Ubuntu/Debian, the non-Go system dependencies can be installed with:

```sh
sudo apt-get install build-essential git make jq ruby ruby-webrick
```

Install Go separately from [go.dev](https://go.dev/doc/install) to get the version required by `go.mod`.

### Windows

Install Go, Git, GNU Make, Ruby, and `jq`, and ensure their commands are available on `PATH`. Install WEBrick with `gem install webrick`. The Windows CI runs `go test -count=1 ./...` instead of `make test`, because `make test` invokes the Bash-only `script/release-test`; use WSL to run the full Linux `make test` target.

Run the Windows checks with:

```powershell
go vet ./...
go test -count=1 ./...
make build
make test-stub
```

### Linux

With the prerequisites above installed, run the full unit/release checks and stub integration scenarios with:

```sh
make test
make test-stub
```

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
