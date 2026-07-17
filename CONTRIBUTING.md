# Contributing to ANet

Thanks for your interest! ANet is early (v0.1) and moving fast.

## Ground rules

- **Discuss first** for anything protocol-level (wire formats, signatures,
  CIDs): open an issue before a PR. Protocol bytes are forever.
- Bug fixes, docs, tests, and portability fixes are always welcome.

## Development

```sh
./build.sh          # build the anet binary (Go 1.26+, CGO required)
./build.sh --check  # gofmt + go vet + go test
```

- Keep changes `gofmt`-clean; match the existing comment style.
- Tests live next to the code; `internal/daemon` has an in-memory fake Hub
  (`hubfake_test.go`) for end-to-end exercises without a real Hub.

## Licensing of contributions

By submitting a contribution you agree it is provided under the ANet
Community License (see LICENSE, Section 6).

## Security issues

Please do **not** open public issues for vulnerabilities — see
[SECURITY.md](SECURITY.md).
