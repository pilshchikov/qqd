# Contributing to qqd

Thanks for your interest. `qqd` is a small project; the workflow is intentionally simple.

## Quick reference

```bash
git clone https://github.com/pilshchikov/qqd
cd qqd
go build ./cmd/qqd                          # build
go test ./internal/qqd/ -count=1            # unit tests
go test ./internal/qqd/ -count=1 -race      # with race detector (CI does this)
go vet ./...                                # static analysis
make release                                # cross-build linux/darwin x amd64/arm64 into dist/
make install                                # source build + local install
./qqd docs -o docs/cli-reference.md         # regenerate auto CLI docs (CI checks for drift)
```

If you change CLI flags, command help, or add/remove a subcommand, you **must** regenerate `docs/cli-reference.md` and commit it. CI fails on drift.

## Philosophy

`qqd` is a deployment tool. The bar is correctness, not feature breadth. Before opening a PR, please:

1. Prefer a small, well-tested change over a large feature. Deployment tools fail in production - test coverage matters more than line count.
2. If you are adding a feature, add an integration-style test for it. Pure-Go logic tests catch regressions, but the failure modes that bite users are network, SSH, systemd, and runtime issues.

## Code style

- Standard `gofmt` formatting (CI runs `go vet`).
- No external dependencies in `internal/qqd` beyond `golang.org/x/crypto` unless there is a discussion first. The "single binary, zero runtime deps" property is part of the product.
- Tests use `io.Discard` for stdout where colors should be suppressed - the color helpers auto-disable for non-TTY writers.
- Public packages are not part of the contract. Everything lives under `internal/qqd` so the only stable surface is the CLI.

## Commit and PR conventions

- One logical change per PR. Bug fixes, refactors, and new features go in separate PRs.
- Commit subject in imperative mood: `Fix Caddy route reload on TLS re-issue`, not `fixed bug`.
- Reference an issue if one exists.

## Tests

The unit test suite uses fakes for SSH and the container runtime. An integration test suite exists and runs against a real Podman machine; it is opt-in via `QQD_INTEGRATION=1`. See [docs/integration-tests.md](docs/integration-tests.md) for setup, coverage, and gaps.

```bash
go test ./internal/qqd/ -count=1                           # unit tests only
QQD_INTEGRATION=1 go test ./internal/qqd/ -count=1 -v      # unit + integration
```

PR CI runs the unit suite with the race detector, plus `go vet` and generated-doc drift checks. The integration suite is run manually before release changes are merged.

## Releases

Every push to `main` or `master` creates a GitHub release named `vYYYY.MM.DD.<run_number>`.

The release workflow builds linux/darwin x amd64/arm64 binaries with `make release`, writes `checksums.txt`, publishes the release, and marks it as latest. The installer downloads that latest release by default.

## Reporting bugs

Open an issue with:

- `qqd --version`
- The relevant snippet of your config (redact secrets)
- Target OS and runtime (`uname -a`, `podman --version` or `docker --version`)
- Exact command and full output
