## What and why

<!-- What does this change, and why is it needed? Link the issue if there is one. -->

## Tests run

- [ ] `./scripts/ci.sh` (or `SKIP_DOCKER=1 ./scripts/ci.sh`, say which)
- [ ] Windows check (`GOOS=windows go vet ./...`, or a real Windows host for Windows code)
- [ ] Other (describe):

## Checklist

- [ ] The title is a [Conventional Commit](https://www.conventionalcommits.org/) (`feat: ...`, `fix(native): ...`, `docs: ...`)
- [ ] README, examples and `deploy/` updated if behaviour or config changed
- [ ] No secrets, tokens or private host names in the diff
