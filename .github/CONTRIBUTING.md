# Contributing

Report bugs and ideas as GitHub issues. Report security problems as described in
[SECURITY.md](SECURITY.md), not in an issue.

The maintainer writes most code. Pull requests are welcome, but please keep them
small and open an issue first to discuss the change.

Use the Go version in `go.mod`. Build and check your change with:

```sh
make build
make fmt-check test vet lint
```

Use [Conventional Commits](https://www.conventionalcommits.org/) for commit
subjects and pull request titles: `feat`, `fix`, `refactor`, `test`, `ci`, `docs`
or `chore`, for example `fix: keep user keys on upgrade`.
