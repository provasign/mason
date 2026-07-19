# Contributing to Mason

Mason accepts focused harness, provider, safety, UX, documentation, and
benchmark changes. Structural enforcement belongs in the harness, not only in a
prompt. Add tests for allowed, denied, degraded, cancellation, and error paths.

Run `go test ./...`, `go test ./... -race -count=1`, and `go vet ./...`.
Provider or routing claims require multi-trial evidence from the research
harness. Pull requests must state commands run, model/provider impact, security
and retention impact, and compatibility changes. Contributions are Apache-2.0.
