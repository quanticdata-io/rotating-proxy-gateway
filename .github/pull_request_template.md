## What and why

<!-- What does this change and which problem does it solve? Link the issue if there is one. -->

## Checklist

- [ ] `go vet ./...` and `go test ./...` pass
- [ ] `gofmt -l .` prints nothing
- [ ] New behaviour has tests (in-process servers only, no network)
- [ ] No upstream password can reach a log, an error or the status page
- [ ] `CHANGELOG.md` updated under "Unreleased"
