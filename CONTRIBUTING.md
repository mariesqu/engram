# Contributing to engram

Thanks for your interest! This project values correctness over speed — most of
its history is adversarially reviewed PRs, and contributions are held to the
same bar.

## Ground rules

- **Pure Go, `CGO_ENABLED=0`.** The single static binary is a hard guarantee.
  No new Go modules without a strong justification in the PR description.
- **No Docker anywhere in the test path.** Acceptance tests use
  [embedded-postgres](https://github.com/fergusstrange/embedded-postgres).
- **Headless testability.** Engine behavior must be provable by `go test` —
  if a change can only be verified by clicking, it needs a redesign.
- **No test may ever require a real API key or network endpoint** — use
  `httptest` contract servers and recording mocks.

## Before you open a PR

```bash
CGO_ENABLED=0 go build ./...
go vet ./... && go vet -tags acceptance ./...
go test ./... -count=1
go test -tags acceptance ./... -count=1   # full suite, embedded-postgres
gofmt -l .                                # must be clean for files you touched
```

Cross-platform builds must stay green: `GOOS=linux` and `GOOS=darwin` with
`CGO_ENABLED=0`.

## Conventions

- Conventional commits (`feat:`, `fix:`, `test:`, `docs:`, `chore:`).
- Keep PRs focused (~400 changed lines is the house target); split larger work
  into chained PRs.
- Tests ship in the same commit as the code they prove.
- Sacred invariants — touch only with extreme care and full test coverage:
  - `mutation_id` is content-addressed and immutable (derived data like
    embeddings must never enter the sync journal).
  - The embedding privacy gate (`internal/embedding/gated.go`) — every
    provider call goes through it; tests must assert on the recording mock's
    **received texts**, not just call counts.
  - The localstore write queue (`Store.mu`) serialization rationale
    (`internal/localstore/store.go`).

## Architecture orientation

Start with `README.md`, then the archived design documents under
`openspec/archive/` — each completed change carries its proposal, specs,
design decisions, and a closing report. They are the canonical record of *why*
the code is shaped the way it is.

## Security issues

See [SECURITY.md](SECURITY.md) — never open a public issue for a vulnerability.
