# Organizing Go Packages with Hexagonal Architecture

Nov 10, 2026 · @gbchd

## The idea in brief

Give each part of the domain one package that holds its types, its use cases and the interfaces those use cases need. Put every technology in a subpackage that imports the parent, never the reverse. That is hexagonal architecture in Go's own idiom, with no packages named after layers.

The three hexagonal roles map onto packages like this:

- **Core**: a domain package such as `order`. It holds the types, the rules and the `Service` that runs the use cases. It knows nothing about HTTP, SQL or JSON.
- **Ports**: interfaces declared inside `order`, beside the code that uses them. `order.Repository` says what the service needs from storage. There is no `port` package.
- **Adapters**: subpackages such as `order/orderpg` and `order/orderhttp`. Each one translates between one technology and `order`.

One rule holds it together: **imports point inward**. A subpackage imports its parent. The parent never imports a subpackage, and Go rejects the attempt as an import cycle.

&#91;embedded content: package dependencies · cmd, package order, its adapter subpackages\]

`cmd` imports everything. Each subpackage imports `order`, either to call its service or to implement one of its interfaces. `order` imports none of them.

Packages named `domain`, `port` and `service` describe layers, so call sites read `domain.Order` and `port.OrderRepository`. Packages named for what they provide read as `order.Order`, `order.Repository` and `orderpg.NewRepository`. The standard library works the same way: `net/http` keeps its types, its server and its `Handler` interface in one package, with helpers such as `httptest` in subpackages named after the parent.

## Recommended layout

Put binaries in `cmd/` and one package per part of the domain in `internal/`, with that package's adapters as its subpackages. The examples use a small shop in a module named `github.com/acme/shop`.

```text
shop/
├── cmd/
│   └── api/
│       └── main.go              # composition root: build, inject, start
├── internal/
│   ├── order/                   # package order: one part of the domain
│   │   ├── order.go             # Order, Line, Money, Status and their rules
│   │   ├── errors.go            # ErrNotFound, ErrEmpty, ErrNotPending
│   │   ├── service.go           # Service, plus the interfaces it uses
│   │   ├── order_test.go
│   │   ├── service_test.go
│   │   ├── testdata/            # fixtures for the tests in this package
│   │   │   └── orders.json
│   │   ├── orderpg/             # implements order.Repository on Postgres
│   │   │   ├── repository.go
│   │   │   ├── repository_test.go
│   │   │   └── testdata/
│   │   │       └── seed.sql
│   │   ├── orderstripe/         # implements order.PaymentGateway
│   │   ├── ordermem/            # in-memory order.Repository for local runs
│   │   ├── orderhttp/           # HTTP handlers that call order.Service
│   │   │   ├── handler.go
│   │   │   ├── handler_test.go
│   │   │   └── testdata/
│   │   │       ├── place_order.request.json
│   │   │       └── place_order.golden.json
│   │   ├── ordergrpc/           # gRPC server that calls the same service
│   │   └── ordertest/           # shared test helpers, repository contract suite
│   ├── customer/                # another part of the domain, same shape
│   │   ├── customer.go
│   │   ├── service.go
│   │   ├── customerpg/
│   │   └── customerhttp/
│   ├── config/                  # loads and validates configuration
│   ├── logging/                 # builds the *slog.Logger
│   └── postgres/                # opens the pool, transaction helper
├── migrations/
├── go.mod
└── go.sum
```

Hexagonal terms still apply. They name roles inside this tree, not directories:

| Hexagonal term | Where it lives | May import |
| --- | --- | --- |
| Domain model | `order/order.go`, `order/errors.go` | Standard library, without I/O packages |
| Application service | `order.Service` in `order/service.go` | The same package |
| Outbound port | `order.Repository`, `order.PaymentGateway` | The same package |
| Inbound port | The exported methods of `order.Service` | The same package |
| Outbound adapter | `order/orderpg`, `order/orderstripe`, `order/ordermem` | `order`, drivers and SDKs |
| Inbound adapter | `order/orderhttp`, `order/ordergrpc` | `order`, transport libraries |
| Composition root | `cmd/api` | Everything |

Subpackages carry their parent's name as a prefix, the way `net/http/httptest` does. Every package name is then unique in the module, so no import ever needs an alias.

This matches the Go team's guidance for server projects: commands under `cmd/`, logic under `internal/` ([Organizing a Go module](https://go.dev/doc/modules/layout)). A `pkg/` directory is not needed for a service that nobody imports.

## The domain package: types and rules

The `order` package starts with the types and the rules of that part of the business, and it imports nothing but safe parts of the standard library. If you deleted every subpackage, it would still compile and its tests would still pass.

```go
// internal/order/order.go
package order

import "time"

type ID string

type Status string

const (
	StatusPending   Status = "pending"
	StatusPaid      Status = "paid"
	StatusCancelled Status = "cancelled"
)

type Money struct {
	Amount   int64  // minor units, for example cents
	Currency string // ISO 4217 code
}

type Line struct {
	SKU      string
	Quantity int
	Price    Money
}

type Order struct {
	ID         ID
	CustomerID string
	Lines      []Line
	Status     Status
	CreatedAt  time.Time
}

// New validates the invariants of a new order.
func New(id ID, customerID string, lines []Line, now time.Time) (*Order, error) {
	if len(lines) == 0 {
		return nil, ErrEmpty
	}
	return &Order{
		ID:         id,
		CustomerID: customerID,
		Lines:      lines,
		Status:     StatusPending,
		CreatedAt:  now,
	}, nil
}

func (o *Order) Total() Money {
	var total Money
	for _, l := range o.Lines {
		total.Amount += l.Price.Amount * int64(l.Quantity)
		total.Currency = l.Price.Currency
	}
	return total
}

// MarkPaid keeps the state transition rule in one place.
func (o *Order) MarkPaid() error {
	if o.Status != StatusPending {
		return ErrNotPending
	}
	o.Status = StatusPaid
	return nil
}
```

```go
// internal/order/errors.go
package order

import "errors"

var (
	ErrNotFound   = errors.New("order: not found")
	ErrEmpty      = errors.New("order: no lines")
	ErrNotPending = errors.New("order: not pending")
)
```

Rules for these files:

- **No struct tags.** `json:"..."` and `db:"..."` describe a wire format or a table, so they belong on types in the subpackages.
- **Behavior lives on the types.** A rule such as "only a pending order can be paid" is a method, not a check repeated in the service.
- **Constructors guard invariants.** `order.New` rejects an empty order, so nothing downstream has to.
- **Time and IDs come in as arguments.** These types never call `time.Now()` or generate a UUID, which keeps tests deterministic.
- **Errors are defined here.** Subpackages translate them, for example `order.ErrNotFound` to HTTP 404.

`Money` sits in `order` while only orders use it. When a second domain package needs it, move it to a small package of its own, for example `internal/money`.

## Interfaces beside the code that uses them

The interfaces a service depends on are declared in its own package, directly above it. Each one is shaped by what the service needs, not by what a technology offers. These are the outbound ports.

```go
// internal/order/service.go
package order

import (
	"context"
	"fmt"
	"time"
)

// Repository is what the service needs from storage.
type Repository interface {
	Save(ctx context.Context, o *Order) error
	Find(ctx context.Context, id ID) (*Order, error)
}

// PaymentGateway is what the service needs from a payment provider.
type PaymentGateway interface {
	Charge(ctx context.Context, id ID, amount Money) error
}
```

The inbound side follows the same rule. The package that calls the service declares the interface it needs, and `*order.Service` satisfies it implicitly:

```go
// internal/order/orderhttp/handler.go
package orderhttp

// Service is the part of order.Service that the handlers call.
type Service interface {
	Place(ctx context.Context, in order.PlaceInput) (*order.Order, error)
	Pay(ctx context.Context, id order.ID) error
}
```

Rules for these interfaces:

- **The consumer declares the interface.** `order` declares `Repository` because `Service` uses it. The `orderpg` subpackage only implements it.
- **Signatures use the package's own types.** No `*sql.Tx`, `*http.Request` or tagged structs. `context.Context` is fine: it carries cancellation, not technology.
- **Keep them small.** One to three methods is typical. Split an interface when two use cases need different halves of it.
- **Use a function for a one-call dependency.** The service takes `now func() time.Time` and `newID func() ID` instead of `Clock` and `IDGenerator` interfaces.
- **Errors are the package's errors.** `Find` returns `order.ErrNotFound`, never `sql.ErrNoRows`.
- **No interface without a consumer.** `Order` and `Money` are plain structs, and `Service` is a concrete type.

## The service

`order.Service` runs the use cases: load through an interface, call behavior on the types, persist through an interface. It lives in the same file as its interfaces, contains no SQL, HTTP or JSON, and receives every dependency through its constructor.

```go
// internal/order/service.go (continued)

type Service struct {
	repo     Repository
	payments PaymentGateway
	now      func() time.Time
	newID    func() ID
}

func NewService(repo Repository, payments PaymentGateway, now func() time.Time, newID func() ID) *Service {
	return &Service{repo: repo, payments: payments, now: now, newID: newID}
}

type PlaceInput struct {
	CustomerID string
	Lines      []Line
}

func (s *Service) Place(ctx context.Context, in PlaceInput) (*Order, error) {
	o, err := New(s.newID(), in.CustomerID, in.Lines, s.now())
	if err != nil {
		return nil, err
	}
	if err := s.repo.Save(ctx, o); err != nil {
		return nil, fmt.Errorf("save order: %w", err)
	}
	return o, nil
}

func (s *Service) Pay(ctx context.Context, id ID) error {
	o, err := s.repo.Find(ctx, id)
	if err != nil {
		return err
	}
	if err := o.MarkPaid(); err != nil {
		return err
	}
	if err := s.payments.Charge(ctx, o.ID, o.Total()); err != nil {
		return fmt.Errorf("charge order %s: %w", o.ID, err)
	}
	return s.repo.Save(ctx, o)
}

func (s *Service) Get(ctx context.Context, id ID) (*Order, error) {
	return s.repo.Find(ctx, id)
}
```

Notice what the service decides and what it delegates. The order of steps is the service's job. The rule about which orders can be paid stays in `Order.MarkPaid`.

Because types, service and interfaces share a package, nothing here needs a qualifier. From outside, the same names read as `order.Service`, `order.PlaceInput` and `order.Repository`.

### Transactions

A use case that writes through several repositories needs a transaction, but `*sql.Tx` must not appear in `order`. Declare one more small interface and let the database subpackage implement it:

```go
// internal/order/service.go
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}
```

The Postgres implementation opens the transaction, stores it in the context it passes to `fn`, and commits or rolls back on return. Its repositories look for that transaction in the context before falling back to the pool.

## Adapters as subpackages

An adapter is a subpackage of the domain package it serves, named with the parent's name followed by its technology. It imports its parent, owns its own data shapes, and never imports another adapter.

### Inbound: orderhttp

The handler decodes a request, maps it to `order` types, calls the service, and maps the result back.

```go
// internal/order/orderhttp/handler.go
package orderhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/acme/shop/internal/order"
)

// Service is the part of order.Service that the handlers call.
type Service interface {
	Place(ctx context.Context, in order.PlaceInput) (*order.Order, error)
	Pay(ctx context.Context, id order.ID) error
}

type Handler struct {
	orders Service
}

func NewHandler(orders Service) *Handler {
	return &Handler{orders: orders}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /orders", h.place)
	mux.HandleFunc("POST /orders/{id}/pay", h.pay)
}

// These types carry the JSON tags so order.Order does not have to.
type lineJSON struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type placeRequest struct {
	CustomerID string     `json:"customer_id"`
	Lines      []lineJSON `json:"lines"`
}

type orderResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Total  int64  `json:"total"`
}

func (req placeRequest) toInput() order.PlaceInput {
	lines := make([]order.Line, 0, len(req.Lines))
	for _, l := range req.Lines {
		lines = append(lines, order.Line{
			SKU:      l.SKU,
			Quantity: l.Quantity,
			Price:    order.Money{Amount: l.Amount, Currency: l.Currency},
		})
	}
	return order.PlaceInput{CustomerID: req.CustomerID, Lines: lines}
}

func (h *Handler) place(w http.ResponseWriter, r *http.Request) {
	var req placeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	o, err := h.orders.Place(r.Context(), req.toInput())
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(orderResponse{
		ID:     string(o.ID),
		Status: string(o.Status),
		Total:  o.Total().Amount,
	})
}

func (h *Handler) pay(w http.ResponseWriter, r *http.Request) {
	id := order.ID(r.PathValue("id"))
	if err := h.orders.Pay(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeError is the one place that maps order errors to status codes.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, order.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, order.ErrEmpty):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, order.ErrNotPending):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
```

The method patterns (`"POST /orders"`) and `r.PathValue` need Go 1.22 or later. With chi, gin or echo only this package changes.

### Outbound: orderpg

The repository implements `order.Repository`, keeps a row type that mirrors the table, and converts driver errors into `order` errors.

```go
// internal/order/orderpg/repository.go
package orderpg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/acme/shop/internal/order"
)

// Compile-time proof that Repository implements the interface.
var _ order.Repository = (*Repository)(nil)

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// row mirrors the table, not order.Order.
type row struct {
	ID         string
	CustomerID string
	Status     string
	CreatedAt  time.Time
}

func (r *Repository) Find(ctx context.Context, id order.ID) (*order.Order, error) {
	var rw row
	err := r.db.QueryRowContext(ctx,
		`SELECT id, customer_id, status, created_at FROM orders WHERE id = $1`,
		string(id),
	).Scan(&rw.ID, &rw.CustomerID, &rw.Status, &rw.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, order.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find order %s: %w", id, err)
	}
	lines, err := r.findLines(ctx, id)
	if err != nil {
		return nil, err
	}
	return &order.Order{
		ID:         order.ID(rw.ID),
		CustomerID: rw.CustomerID,
		Lines:      lines,
		Status:     order.Status(rw.Status),
		CreatedAt:  rw.CreatedAt,
	}, nil
}

// Save and findLines follow the same pattern and are omitted here.
```

Rules for adapters:

- **Map at the boundary.** Request, response and row types stay unexported inside the subpackage. Only `order` types cross into the parent.
- **Translate errors in both directions.** `sql.ErrNoRows` becomes `order.ErrNotFound` on the way in, and that becomes 404 on the way out.
- **Constructors return concrete structs.** `orderpg.NewRepository` returns `*Repository`. The wiring code passes it where an `order.Repository` is expected.
- **No adapter imports another adapter.** If `orderhttp` needs data, it asks the service, which asks `orderpg` through `order.Repository`.
- **No business decisions.** An `if` about order status in a handler or a SQL query is a rule that leaked out of `order`.

## Wiring in cmd

`main` is the composition root: the only package that imports a domain package together with its adapters. It builds the outbound adapters, hands them to the service, hands the service to the inbound adapters, and starts the process.

```go
// cmd/api/main.go
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/acme/shop/internal/config"
	"github.com/acme/shop/internal/logging"
	"github.com/acme/shop/internal/order"
	"github.com/acme/shop/internal/order/orderhttp"
	"github.com/acme/shop/internal/order/orderpg"
	"github.com/acme/shop/internal/order/orderstripe"
	"github.com/acme/shop/internal/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	slog.SetDefault(logging.New(cfg.LogLevel))

	db, err := postgres.Open(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	orders := order.NewService(
		orderpg.NewRepository(db),
		orderstripe.NewGateway(cfg.StripeKey),
		time.Now,
		func() order.ID { return order.ID(uuid.NewString()) },
	)

	mux := http.NewServeMux()
	orderhttp.NewHandler(orders).Register(mux)

	slog.Info("listening", "addr", cfg.Addr)
	return http.ListenAndServe(cfg.Addr, mux)
}
```

Guidelines for wiring:

- **No import aliases.** Every subpackage name is unique in the module, and `orderpg.NewRepository` says at the call site which part of the domain it serves.
- **Plain constructors are enough.** The compiler checks the whole dependency graph, and a missing dependency is a build error. Reach for a tool such as Wire only when the wiring itself becomes hard to read.
- **Keep `main` free of logic.** The `run() error` shape lets deferred cleanup execute and keeps error handling in one place.
- **Swap adapters here and nowhere else.** A local run can pass `ordermem.NewRepository()` instead of the Postgres one without touching `order`.
- **One directory per binary.** `cmd/worker` or `cmd/migrate` build different adapters around the same domain packages.

## Shared technical packages

Config loading, logging setup and the database pool each get a small package directly under `internal/`, named for what it provides. There is no `utils` package, and domain packages never import these.

| Package | Provides | Imported by |
| --- | --- | --- |
| `internal/config` | `config.Load()`, returning a typed struct read from the environment | `cmd` only |
| `internal/logging` | `logging.New(level)`, returning a configured `*slog.Logger` | `cmd` only |
| `internal/postgres` | `postgres.Open(url)`, pool settings, the transaction helper | `cmd` and the `*pg` adapters |

Rules for these packages:

- **`cmd` reads config, everything else receives values.** `orderstripe.NewGateway(cfg.StripeKey)` takes a string, not a `config.Config`. A package that imports `config` cannot be tested without the whole configuration.
- **Domain packages do not log.** They return errors with context, and the adapter or `main` logs each error once. If a service does need a logger, pass a `*slog.Logger` to its constructor. It is a standard library type, so the dependency rule still holds.
- **Technical packages never import domain packages.** They sit at the bottom of the import graph, which keeps them reusable and free of cycles.
- **A helper used by one package stays in that package.** Keep it unexported in its own file, and promote it only when a second package needs it.
- **Check the standard library first.** `slices`, `maps`, `cmp`, `errors` and `log/slog` cover most of what a `utils` package used to hold.
- **Group them only when they crowd the tree.** With many technical packages, move them under one directory such as `internal/platform/` so the domain packages stand out. Names and rules stay the same.

## Naming conventions

Name a package for what it provides, and let the package name do half the work of every identifier inside it. Callers always read `package.Name`, so a layer name or a repeated word is noise.

| What | Convention | Example |
| --- | --- | --- |
| Domain package | A noun from the business, singular, lowercase | `order`, `customer`, `invoice` |
| Adapter subpackage | Parent name plus technology, as `httptest` is to `http` | `order/orderpg`, `order/orderstripe`, `order/orderhttp` |
| Technical package | What it provides, directly under `internal/` | `config`, `logging`, `postgres` |
| Layer names | Never a package name | No `domain`, `service`, `port`, `adapter`, `models` |
| Catch-all packages | Avoid them; they attract unrelated code and create cycles | No `util`, `common`, `helpers` |
| Import aliases | Not needed: every package name is unique in the module | `orderpg` and `customerpg`, never two `postgres` |
| Types | Lean on the package name, do not repeat it | `order.Service`, `order.Repository`, `order.ID`, not `order.OrderService` |
| The main type | May share the package's name, as `time.Time` does | `order.Order` |
| Constructors | `New` for the main type, `New<Type>` for the rest | `order.New`, `order.NewService`, `orderpg.NewRepository` |
| Errors | `Err` prefix, message prefixed with the package | `order.ErrNotFound`, `"order: not found"` |
| Interfaces | A role noun, or an `-er` name for one method | `Repository`, `PaymentGateway`, `Notifier` |
| Adapter data types | Unexported, named for what they mirror | `placeRequest`, `orderResponse`, `row` |
| File names | Lowercase, one main concept per file | `order.go`, `service.go`, `errors.go`, `handler.go` |
| Test helpers | A `<pkg>test` subpackage | `order/ordertest` |
| Fixtures | A `testdata` directory beside the tests that read it | `order/orderhttp/testdata/place_order.golden.json` |

Split a file when it holds two concepts, not when it reaches a line count. Split a package when part of it has a different reason to change, which usually means it is a different part of the domain.

## Testing each part

The payoff of the layout is that most tests need no infrastructure. Only outbound adapters are tested against the real thing.

| What | Kind of test | Test doubles | Needs infrastructure |
| --- | --- | --- | --- |
| `order` types | Table-driven unit tests of rules | None | No |
| `order.Service` | Unit test per use case | Fakes of `Repository` and `PaymentGateway`, fixed `now` and `newID` | No |
| `order/orderhttp` | `httptest` requests against the handler, golden responses | A stub of `orderhttp.Service` | No |
| `order/orderpg` | Integration tests | None | Yes, for example Postgres in a container |

A service test builds the service with fakes and asserts on behavior:

```go
// internal/order/service_test.go
package order_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/acme/shop/internal/order"
)

type fakeRepo struct{ byID map[order.ID]*order.Order }

func (f *fakeRepo) Save(_ context.Context, o *order.Order) error {
	f.byID[o.ID] = o
	return nil
}

func (f *fakeRepo) Find(_ context.Context, id order.ID) (*order.Order, error) {
	o, ok := f.byID[id]
	if !ok {
		return nil, order.ErrNotFound
	}
	return o, nil
}

type fakePayments struct{ charges int }

func (f *fakePayments) Charge(context.Context, order.ID, order.Money) error {
	f.charges++
	return nil
}

func TestPay_ChargesOnlyOnce(t *testing.T) {
	ctx := context.Background()
	payments := &fakePayments{}
	svc := order.NewService(
		&fakeRepo{byID: map[order.ID]*order.Order{}},
		payments,
		func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		func() order.ID { return "order-1" },
	)

	o, err := svc.Place(ctx, order.PlaceInput{
		CustomerID: "cust-1",
		Lines: []order.Line{
			{SKU: "book", Quantity: 2, Price: order.Money{Amount: 1500, Currency: "EUR"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Pay(ctx, o.ID); err != nil {
		t.Fatal(err)
	}

	err = svc.Pay(ctx, o.ID)
	if !errors.Is(err, order.ErrNotPending) {
		t.Fatalf("second payment: got %v, want ErrNotPending", err)
	}
	if payments.charges != 1 {
		t.Fatalf("charges: got %d, want 1", payments.charges)
	}
}
```

Practices that keep this honest:

- **Test through the exported API.** `package order_test` sees `order` as a subpackage does, so the tests exercise the same surface the adapters use.
- **Prefer fakes to generated mocks.** A fake with a map behaves like a repository. A mock that asserts call order couples the test to the implementation.
- **Reuse `order/ordermem` as the fake.** An external test package can import a subpackage without creating a cycle, so the in-memory repository can replace `fakeRepo`.
- **Tag integration tests.** Start the file with `//go:build integration` and run it with `go test -tags=integration ./internal/order/orderpg/...`.
- **Share a contract test.** Put one suite in `order/ordertest` that takes an `order.Repository`, and run it against both `ordermem` and `orderpg`. It proves the fake and the real adapter agree.

### Fixtures in testdata

Each package keeps its fixtures in a `testdata` directory beside the tests that read them. The `go` tool ignores any directory with that name, so its contents are never compiled or listed as a package. A test runs with its package directory as the working directory, so a relative path is enough.

| Directory | Holds |
| --- | --- |
| `order/testdata/` | Sample orders as JSON for table-driven tests of the rules |
| `order/orderhttp/testdata/` | Request bodies and golden responses |
| `order/orderpg/testdata/` | SQL seed files loaded before integration tests |

A golden-file test compares the handler's output with a stored response and rewrites it on demand:

```go
// internal/order/orderhttp/handler_test.go
package orderhttp_test

import (
	"bytes"
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/acme/shop/internal/order"
	"github.com/acme/shop/internal/order/orderhttp"
)

var update = flag.Bool("update", false, "rewrite golden files")

type stubService struct{}

func (stubService) Place(_ context.Context, in order.PlaceInput) (*order.Order, error) {
	return order.New("order-1", in.CustomerID, in.Lines, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
}

func (stubService) Pay(context.Context, order.ID) error { return nil }

func TestPlace_Golden(t *testing.T) {
	body, err := os.ReadFile("testdata/place_order.request.json")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	orderhttp.NewHandler(stubService{}).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(body)))

	const golden = "testdata/place_order.golden.json"
	if *update {
		if err := os.WriteFile(golden, rec.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("response differs from %s:\n%s", golden, rec.Body.Bytes())
	}
}
```

Regenerate the golden file with `go test ./internal/order/orderhttp -update`, then review the diff like any other code change.

Rules for fixtures:

- **A package reads only its own `testdata`.** A path that starts with `../` couples two packages through files that no compiler checks.
- **Share fixtures as code, not files.** When several packages need the same sample order, add a helper to `order/ordertest` that returns an `order.Order`.
- **Commit golden files.** They are the expected output, and a reviewer should see them change.
- **No production data.** Fixtures are invented, small and safe to publish.

## Enforcing the boundaries

With adapters as subpackages the compiler enforces most of the dependency rule, and a linter or a test covers the rest. The gap that remains is a domain package importing a technology directly.

| Mechanism | Stops | Does not stop |
| --- | --- | --- |
| `internal/` directory | Other modules importing your packages | Any import inside the module |
| Import cycle ban | `order` importing `order/orderpg` or `order/orderhttp`, since both import `order` | `order` importing `database/sql`, `config` or `customer/customerpg` |
| `depguard` linter | Any denied import in matching files | Imports you did not list |
| Architecture test | A domain package depending on any subpackage | Imports of the standard library or of technical packages |

### Linter rule

[depguard](https://github.com/OpenPeeDeeP/depguard) runs inside golangci-lint and denies imports per file pattern. The single `*` matches files directly in a domain package and leaves its subpackages alone. This configuration uses the golangci-lint v2 format:

```yaml
# .golangci.yml
version: "2"
linters:
  enable:
    - depguard
  settings:
    depguard:
      rules:
        domain:
          files:
            - "**/internal/order/*.go"
            - "**/internal/customer/*.go"
          deny:
            - pkg: database/sql
              desc: storage belongs in a subpackage such as order/orderpg
            - pkg: net/http
              desc: transport belongs in a subpackage such as order/orderhttp
            - pkg: encoding/json
              desc: wire formats belong in a subpackage
            - pkg: github.com/acme/shop/internal/config
              desc: pass values through constructors instead
            - pkg: github.com/acme/shop/internal/postgres
              desc: only cmd and the pg adapters use the pool
```

### Architecture test

A test with no extra tooling asks the `go` command for every dependency of each domain package. It fails if one of them is a subpackage, whether its own or another domain's:

```go
// internal/arch_test.go
package internal_test

import (
	"os/exec"
	"strings"
	"testing"
)

const root = "github.com/acme/shop/internal/"

// Add a name here when you add a part of the domain.
var domainPackages = []string{"order", "customer"}

func TestDomainPackagesImportNoAdapters(t *testing.T) {
	for _, name := range domainPackages {
		out, err := exec.Command("go", "list", "-deps", root+name).Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, dep := range strings.Fields(string(out)) {
			sub, ok := strings.CutPrefix(dep, root)
			if ok && strings.Contains(sub, "/") {
				t.Errorf("%s depends on %s", name, dep)
			}
		}
	}
}
```

Run both in CI. The linter gives a precise message at the offending import, and the test catches indirect paths.

## Growing the layout

Grow by adding domain packages side by side, never by adding layers. The shape of each package stays the same at every size.

| Stage | Layout | Signal to move on |
| --- | --- | --- |
| One part of the domain | `internal/order` with its subpackages | A second noun appears with its own rules and its own storage |
| Several parts | Sibling packages such as `order`, `customer`, `invoice`, each with its own subpackages | One part needs its own release cycle or its own team |
| Independent parts | The package moves to its own module or service, subpackages included |  |

Moving a domain package out is cheap, because its adapters travel with it and nothing else imports them.

### When a domain package gets too big

Split files first, and extract a new package only when a distinct concept has emerged inside it. Size alone is not a reason: `net/http` is one very large package and reads well.

Take these steps in order, and stop at the first one that solves the problem:

1. **Split files, not the package.** Break `service.go` into one file per use case, such as `place.go`, `pay.go` and `refund.go`. Nothing outside the package changes.
2. **Split the service type.** When `Service` has a dozen methods and six dependencies, make two types in the same package, for example `order.Service` and `order.RefundService`. Each one declares only the interfaces it uses.
3. **Extract a sibling domain package.** Do this when a cluster of code has its own vocabulary, its own rules and its own storage.

These signs show that a cluster is ready to leave:

- Type and file names share a prefix: `ShipmentStatus`, `ShipmentRepository`, `shipment_*.go`.
- It has its own tables.
- Its tests never touch the rest of the package.
- It changes for different reasons, or a different person owns it.

The extracted package is a sibling with its own adapters, not a child of the package it came from:

```text
internal/
├── order/
│   └── orderpg/
└── shipment/          # was order/shipment_*.go
    └── shipmentpg/
```

Subpackages stay reserved for adapters, which keeps the import rule easy to read. `shipment` reaches `order` as described in the next section: through an interface it declares, or a one-way import of `order.ID`.

Two splits make things worse:

- **Splitting by layer.** `order/model` and `order/service` bring back packages named after layers.
- **Splitting code that calls itself constantly.** If the two new packages would import each other, they are one concept. Keep them together, or move the shared value types such as `Money` into a small third package.

### When one domain package needs another

Declare an interface in the consumer, exactly as for a database. Here `order` needs a fact about customers:

```go
// internal/order/service.go

// CustomerChecker is what the order service needs to know about customers.
type CustomerChecker interface {
	CanOrder(ctx context.Context, customerID string) (bool, error)
}
```

`*customer.Service` satisfies it by having that method, and `main` passes it to `order.NewService`. The `order` package never imports `customer`.

Importing another domain package for its types is acceptable in one direction, for example `order` using `customer.ID`. An import cycle between two domain packages means they are one concept, or they share a third concept that deserves its own package.

### One API over several domain packages

Each HTTP subpackage, such as `orderhttp` and `customerhttp`, exposes `Register(mux)`, and `main` calls them all on one mux. Shared middleware such as authentication wraps that mux in `main`.

When the API is generated from a single specification, such as OpenAPI or protobuf, one top-level transport package is the better fit. `internal/api` then imports every domain package. It is still an inbound adapter, and the rule is unchanged: it imports the domain packages, and they never import it.

## Common mistakes

Most failures of this style come from technology leaking into a domain package or from structure borrowed from other languages.

| Mistake | Why it hurts | Fix |
| --- | --- | --- |
| Packages named `domain`, `service`, `port`, `models` | Unrelated concepts share a package, and every call site stutters | One package per part of the domain |
| `json` or `db` tags on domain structs | An API or schema change forces a domain change | Separate request and row types, mapped in the subpackage |
| An interface that accepts `*sql.Tx` or returns `*sql.Rows` | The domain package now depends on the database driver | Its own types in signatures; a `Transactor` interface for transactions |
| Interfaces declared beside their implementation | The consumer imports the adapter to get the interface | Declare the interface in the package that uses it |
| One repository interface with twenty methods | Every fake must implement all of them | Small interfaces, split by what each use case needs |
| Business rules in handlers or SQL | The rule cannot be tested without infrastructure | Move it to a method on the type or to the service |
| An interface for every struct | Indirection with no second implementation | Interfaces only where a subpackage plugs in |
| `utils` or `common` packages | They collect unrelated code and cause import cycles | Name packages for what they provide |
| One adapter calling another | Hidden coupling that bypasses the service | Route the call through an interface in the domain package |
| A new package for every small type | Dozens of tiny packages and cycles between them | Start with one package per noun; split when it has two reasons to change |

## Review checklist

Use these questions when reviewing a change:

- Is every package named for a part of the domain or a technology, never a layer?
- Does each domain package import only the standard library and, at most, other domain packages?
- Is every interface declared in the package that calls it?
- Do interface signatures use only the package's own types and `context.Context`?
- Are struct tags limited to types in adapter subpackages?
- Are driver errors translated to the domain package's errors, and those to status codes in the transport subpackage?
- Is every concrete adapter constructed in `cmd/`, and nowhere else?
- Is `config` imported by `cmd/` only, with values passed down through constructors?
- Does each test read fixtures only from its own package's `testdata`?
- Can the new use case be tested with fakes and no running database?

## Sources

- [Organizing a Go module](https://go.dev/doc/modules/layout), the Go team's layout guidance for packages, commands and servers
- [depguard](https://github.com/OpenPeeDeeP/depguard), the import-restriction linter and its configuration format

