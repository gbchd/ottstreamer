# Package layout

This module uses hexagonal architecture expressed as Go packages. Three terms carry the rules:

- **Domain package**: `internal/<noun>` (`order`, `customer`). Holds one part of the business: its types, its rules, its `Service`, and the interfaces that service uses.
- **Adapter subpackage**: `internal/<noun>/<noun><tech>` (`orderpg`, `orderhttp`, `orderstripe`). Wraps one technology for its parent domain package.
- **Composition root**: `cmd/<binary>/main.go`. Builds adapters and passes them to services.

## Rules

1. Imports point inward. An adapter subpackage imports its domain package. A domain package imports the standard library (minus `database/sql`, `net/http`, `encoding/json`) and, at most, other domain packages.
2. Name every package for a business noun or a technology. Layer names (`domain`, `service`, `port`, `models`, `utils`) are not package names here.
3. Declare each interface in the package that calls it, with one to three methods, using only that package's types and `context.Context`.
4. Name types against their package: `order.Service`, `order.Repository`, `order.ID`, `orderpg.NewRepository`.
5. Keep struct tags, request/response types and row types unexported inside adapter subpackages, and map them to domain types at the boundary.
6. Translate errors at the boundary: outbound adapters turn driver errors into the domain package's `Err...` values, inbound adapters turn those into status codes.
7. Business rules are methods on domain types or steps in the `Service`. Handlers and SQL only translate.
8. Construct concrete adapters in `cmd/` only. `internal/config` is imported by `cmd/` only; every other package receives plain values through its constructor.
9. Time and IDs enter a domain package as arguments or injected funcs (`now func() time.Time`).
10. Shared technical code gets its own package under `internal/`, named for what it provides (`config`, `logging`, `postgres`).
11. Fixtures live in `testdata/` beside the tests that read them. Fixtures shared across packages are helper funcs in `<noun>/<noun>test`.

## Adding code

- New use case: a method on the domain package's `Service`, in its own file (`refund.go`), with a test that uses fakes.
- New technology for an existing domain package: a new adapter subpackage that implements an interface declared in the domain package.
- New business noun with its own rules and storage: a new sibling domain package under `internal/`, with its own adapter subpackages.

## Gate: run before every commit

Run the full gate from the repo root before each `git commit`, and commit only when every step is green:

```sh
test -z "$(gofmt -l .)"    # formatting
go vet ./...
go build ./...
golangci-lint run ./...    # depguard enforces rule 1
go test -race ./...        # includes internal/arch_test.go
```

A red step means fix the cause, then rerun the whole gate. Every new behavior in a domain package has a test that runs without a database.

## Reference

Read `docs/architecture.md` before any of these: creating a domain package, splitting a package that has grown, adding a transaction across repositories, or connecting one domain package to another.
