# What and why

<!-- What changes, and the problem it solves. Link the issue if there is one. -->

# Checklist

- [ ] Title follows conventional commits (`feat:`, `fix:`, `test:`, `docs:`, `chore:`)
- [ ] `CGO_ENABLED=0 go build ./...` succeeds
- [ ] `go vet ./...` is clean
- [ ] `go test ./... -count=1` passes
- [ ] `gofmt -l .` is clean for the files I touched
- [ ] PR stays close to the ~400 changed-line house target (split larger work into chained PRs)
- [ ] Tests ship in the same commit as the code they prove
- [ ] No new Go module dependency (or the PR description justifies it)

# Acceptance tests

<!--
Are acceptance tests relevant to this change (sync, central store, Postgres,
end-to-end daemon paths)? If so, run them locally and say so:
    go test -tags acceptance ./... -count=1
They are excluded from CI on purpose. If not relevant, write "not relevant".
-->

# Notes for the reviewer

<!-- Sacred invariants touched (mutation_id immutability, the embedding privacy
     gate, the localstore write-queue serialization)? Say so explicitly. -->
