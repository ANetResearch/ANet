# Contributing to ANet

Thanks for your interest! ANet is early (v0.1) and moving fast.

## Ground rules

- **Discuss first** for anything protocol-level (wire formats, signatures,
  CIDs): open an issue before a PR. Protocol bytes are forever.
- Bug fixes, docs, tests, and portability fixes are always welcome.

## Development

```sh
./build.sh          # build the anet binary (Go 1.26+, pure Go, CGO off)
./build.sh --check  # gofmt + go vet + go test, and the build-tag checks
CGO_ENABLED=1 go test -race -timeout 45m ./...   # the race detector
```

- The race run needs `-timeout`: `internal/daemon` alone takes about 11–12
  minutes under `-race`, past `go test`'s default limit of 10 minutes per
  test binary (the run then fails as "test timed out" with no failing test).
  CI's race job passes the same flag; `build.sh --check` does not run `-race`.

- Keep changes `gofmt`-clean; match the existing comment style.
- Build tags come in two directions. **Subtractive** — in by default,
  `-tags no_<name>` removes it: `no_anetlink no_p2p no_blackboard no_org
  no_cas no_service no_mcp no_x402 no_a2a`. **Additive** — absent by
  default, `-tags <name>` adds it: `shell taskboard`. `go test ./...` does
  not see code behind an additive tag, so run `go vet`/`go test` with
  `-tags shell,taskboard` too. `bash scripts/tagcheck.sh all` checks every
  tag by symbol count in its own direction; the lists live in that script,
  and CI and `build.sh --check` both call it. A new optional module gets
  its tag there and a row in `.github/workflows/ci.yml`.
- Tests live next to the code; `internal/daemon` has an in-memory fake Hub
  (`hubfake_test.go`) for end-to-end exercises without a real Hub.

## Licensing of contributions

By submitting a contribution you agree it is provided under the ANet
Community License (see LICENSE, Section 6).

## Security issues

Please do **not** open public issues for vulnerabilities — see
[SECURITY.md](SECURITY.md).
