# stripe-mock — Architecture & Exploration Index

> A spec-driven mock of the Stripe HTTP API. It accepts the same requests as the
> real API, validates their parameters against the **Stripe OpenAPI spec**, and
> returns **type-correct but hardcoded** responses generated from fixtures. It is
> **stateless** by default — data you POST is validated and then discarded, never
> stored. **This fork** adds an opt-in stateful layer for PaymentIntents (see
> [Stateful PaymentIntent layer](#stateful-paymentintent-layer-fork-extension)).
>
> Use this document as a map: skim the [Request Lifecycle](#request-lifecycle) to
> understand the flow, then jump to the [File Index](#file-index) to find the
> exact file/function for whatever you're changing.

---

## Table of Contents

1. [What it is (and isn't)](#what-it-is-and-isnt)
2. [The 10-second mental model](#the-10-second-mental-model)
3. [Request Lifecycle](#request-lifecycle)
4. [File Index](#file-index) ← *the map*
5. [Core Data Model (`spec` types)](#core-data-model)
6. [Subsystem Deep-Dives](#subsystem-deep-dives)
7. [Stateful PaymentIntent layer (fork extension)](#stateful-paymentintent-layer-fork-extension)
8. [Build, Test & Run](#build-test--run)
9. ["Where do I look to change X?"](#where-do-i-look-to-change-x)
10. [Glossary](#glossary)

---

## What it is (and isn't)

| ✅ It does | ❌ It does not |
|---|---|
| Knows every Stripe API URL + signature (404s on unknown URLs) | Reproduce real API **behavior** |
| Validates request params against JSON Schema | Persist state (it's stateless) |
| Returns type-correct JSON/PDF from fixtures | Return *realistic* values |
| Reflects valid input params back into the response | Support old API versions (locked to the bundled spec) |
| Serves over HTTP **and** HTTPS (HTTP/2) from a single self-contained binary | Let you trigger specific error/test-card responses |

Primary use: SDK test suites (stripe-ruby, stripe-go, …) asserting they hit the
right URL with the right params. **Not** a substitute for testing against Stripe testmode.

---

## The 10-second mental model

```
OpenAPI spec (embedded JSON)  ─┐
                               ├─►  StubServer  ──►  validate request  ──►  generate response from fixtures
Fixtures (embedded JSON)      ─┘                          (jsval)              (DataGenerator + datareplacer)
```

Everything is driven by **two embedded JSON files** compiled into the binary:
- `spec3.json` — the OpenAPI 3 spec (paths, operations, schemas).
- `fixtures3.json` — sample resource objects keyed by `x-resourceId`.

There is almost no hand-written API logic; routes, validation, and responses are
all derived from the spec at startup and per-request.

---

## Request Lifecycle

End-to-end path of an HTTP request (entrypoint `StubServer.HandleRequest`,
`server/server.go:229`):

```
                    main.go
                       │  loads embedded spec+fixtures, builds StubServer,
                       │  wires net/http mux + DoubleSlashFixHandler
                       ▼
┌────────────────────────────────────────────────────────────────────────────┐
│ StubServer.HandleRequest  (server/server.go)                                 │
│                                                                              │
│  1. validateAuth()            – require "Bearer sk_test_…" / rk_test_…       │
│  2. (optional) Stripe-Version strict check                                   │
│  3. reflect Idempotency-Key, set Request-Id header                           │
│  4. routeRequest()            – regex-match path → stubServerRoute + IDs ─────┼─► routing table built once in
│  5. pick 200 response schema  – application/json or application/pdf          │     initializeRouter() at boot
│  6. param.ParseParams(r)      – query+body → nested map[string]interface{} ──┼─► param/ subsystem
│  7. validateAndCoerceRequest()– coercer.CoerceParams + jsval validator ──────┼─► param/coercer + spec/validate
│  8. extractExpansions()       – parse `expand[]` into ExpansionLevel tree     │
│  9. DataGenerator.Generate()  – walk schema+fixtures → response JSON ─────────┼─► server/generator.go
│ 10. writeResponse()           – marshal, set Stripe-Mock-Version, write       │
└────────────────────────────────────────────────────────────────────────────┘
```

Two phases worth distinguishing:

- **Boot (once):** `LoadSpec` + `LoadFixtures` → `NewStubServer` → `initializeRouter`
  compiles every spec path into a regex, builds a `jsval` validator per operation,
  and sorts routes so static paths beat dynamic ones (e.g. `/v1/invoices/upcoming`
  wins over `/v1/invoices/{invoice}`).
- **Per request:** the 10 steps above.

> **Fork note:** two extra interception points wrap this flow for the stateful
> PaymentIntent layer — control-plane requests (`/v1/_mock/*`) are handled before
> step 1, and stateful PaymentIntent/Charge handling runs after step 7. Both fall
> through to the generic flow when not applicable. See
> [Stateful PaymentIntent layer](#stateful-paymentintent-layer-fork-extension).

---

## File Index

> Vendored deps (`vendor/`) and embedded JSON blobs omitted. `*_test.go` files
> sit next to each source file and are the best usage examples for that unit.

### Entrypoint & process wiring

| File | Purpose | Key symbols |
|---|---|---|
| `main.go` | CLI flags, port/socket/TLS setup, loads embedded assets, starts HTTP+HTTPS goroutines. | `main`, `options`, `checkConflictingOptions`, `getHTTPListener`, `getNonSecureHTTPSListener`, `getTLSCertificate` |
| `embedded/embedded.go` | `//go:embed` bundles spec, fixtures, beta variants, and TLS cert/key into the binary. | `OpenAPISpec`, `OpenAPIFixtures`, `BetaOpenAPISpec`, `BetaOpenAPIFixtures`, `CertCert`, `CertKey` |

### `server/` — routing, validation orchestration, response generation (the core)

| File | Purpose | Key symbols |
|---|---|---|
| `server/server.go` | The HTTP brain. Defines `StubServer`, builds the routing table, handles each request, auth, content-type/version checks, expansions, response writing. | `StubServer`, `HandleRequest`, `initializeRouter`, `routeRequest`, `compilePath`, `validateAndCoerceRequest`, `validateAuth`, `extractExpansions`/`parseExpansionLevel`, `LoadSpec`, `LoadFixtures`, `DoubleSlashFixHandler`, `ExpansionLevel`, `PathParamsMap` |
| `server/generator.go` | Recursively walks an OpenAPI schema + fixtures to synthesize the JSON response. Handles `$ref`, `anyOf`, lists, search results, expansions, ID substitution, synthetic fallback. | `DataGenerator`, `GenerateParams`, `Generate`, `generateInternal`, `generateListResource`, `generateSearchResultResource`, `generateSyntheticFixture`, `maybeDereference`, `findAnyOfBranch`, `recordAndReplaceIDs`, `distributeReplacedIDs`, `randomID` |
| `server/stateful.go` | **(fork)** Per-session store of stateful resources + a FIFO event queue; session id derived from the API key. | `statefulStore`, `sessionStore`, `putResource`/`getResource`, `enqueueEvent`/`drainEvents`, `sessionID`, `apiKeyFromAuth`, `mergeMap` |
| `server/control.go` | **(fork)** Test-only `/v1/_mock/*` control plane: seed, reset, emit, drain. Resolves a route's `x-resourceId`. | `handleControlRequest`, `handleSeedPaymentIntent`, `handleEmitPaymentIntentEvent`, `generateResourceBase`, `routeResourceID` |
| `server/paymentintents.go` | **(fork)** Stateful PaymentIntent lifecycle + magic test cards + Charge model + event emission. | `maybeHandleStatefulRequest`, `createPaymentIntent`, `applyConfirm`/`applyCapture`/`applyCancel`/`applyUpdate`, `recordPaymentIntentTransition`, `ensureChargeForPI`, `buildEvent`, `magicPaymentMethods` |

### `spec/` — OpenAPI data model + spec→validator/query bridges

| File | Purpose | Key symbols |
|---|---|---|
| `spec/spec.go` | Go structs mirroring the OpenAPI spec; custom `Schema.UnmarshalJSON` that **errors on unknown fields** (keeps stripe-mock honest about spec drift). | `Spec`, `Components`, `Schema`, `Operation`, `Parameter`, `RequestBody`, `Response`, `MediaType`, `Fixtures`, `supportedSchemaFields` |
| `spec/validate.go` | Converts an OpenAPI 3 `Schema` into a `lestrrat-go/jsval` validator; bridges `nullable:true` → JSON-Schema `["type","null"]`. | `GetValidatorForOpenAPI3Schema`, `GetComponentsForValidation`, `getJSONSchemaForOpenAPI3Schema` |
| `spec/query.go` | Synthesizes an object `Schema` from a GET operation's `query` parameters so GET requests can be validated like bodies. | `BuildQuerySchema` |

### `param/` — turn raw HTTP query/body into a typed nested map

| File | Purpose | Key symbols |
|---|---|---|
| `param/param.go` | Orchestrates parsing: pulls query string + form/multipart body, hands off to parser + assembler. | `ParseParams` |
| `param/parser/parser.go` | Parses `key=value&…` into **ordered** key/value pairs (order matters for `items[0]`, `items[1]`). | `ParseFormString` |
| `param/form/form.go` | Tiny shared types to avoid import cycles. | `Pair`, `Values` |
| `param/nestedtypeassembler/nestedtypeassembler.go` | Expands Rack-style bracket/dot keys (`items[0][plan]=x`) into nested maps/arrays. | `AssembleParams`, `parseKey`, `buildParamStructure`, `mergeMapsRecursive`, `maybeCollapseArrays` |
| `param/coercer/coercer.go` | Coerces string form values → int/bool/number per the request schema (called from `server.go`, *after* assembly). | `CoerceParams`, `coerceSubSchema`, `coercePrimitiveType`, `parseIntegerIndexedMap` |

### `generator/datareplacer/` — reflect request values into responses

| File | Purpose | Key symbols |
|---|---|---|
| `generator/datareplacer/datareplacer.go` | After generation, overlays request params onto the response **only where name + type match** (so `amount=123` → `"amount":123`). | `DataReplacer`, `ReplaceData`, `replaceDataInternal`, `isSameType`, `maybeDereference` |

### Tooling & ops

| File | Purpose |
|---|---|
| `Makefile` | `build`, `test`, `vet`, `lint` (staticcheck), `check-gofmt`, `docker-*`, and **`update-openapi-spec`** (re-downloads spec+fixtures from `stripe/openapi`). |
| `Dockerfile` / `goreleaser.dockerfile` / `goreleaser.yml` | Container image + release automation (binaries published on tag). |
| `.github/workflows/{ci,release,tag}.yml` | CI (test/vet/lint), release, and tagging pipelines. |
| `scripts/check_gofmt.sh`, `scripts/pre-commit` | gofmt enforcement. |
| `staticcheck.conf`, `.editorconfig` | Lint/format config. |

---

## Core Data Model

`spec.Spec` is the in-memory shape of `spec3.json` (`spec/spec.go`):

```
Spec
├── Info.Version                         // Stripe API version string e.g. "2024-…"
├── Components.Schemas: map[string]*Schema   // reusable resource definitions ($ref targets)
└── Paths: map[Path]map[HTTPVerb]*Operation  // every URL → verb → operation
                                  │
                                  └── Operation
                                      ├── Parameters []*Parameter   // query/path params
                                      ├── RequestBody.Content[mediaType].Schema
                                      └── Responses["200"].Content["application/json"].Schema
```

`Schema` is the workhorse type. Stripe-specific extensions that drive generation:

- `XResourceID` (`x-resourceId`) — fixture lookup key.
- `XExpandableFields` (`x-expandableFields`) — which fields can be `expand`ed.
- `XExpansionResources` (`x-expansionResources`) — the schema to use when a field *is* expanded.
- `AnyOf` — polymorphism (e.g. deleted vs. live resource; expandable id-or-object).

> **Gotcha for spec upgrades:** `Schema.UnmarshalJSON` *rejects* any field not in
> `supportedSchemaFields`. If a spec bump introduces a new `x-…` field, you'll get
> `unsupported field in JSON schema` until you add it to that allowlist
> (`spec/spec.go:63`).

`spec.Fixtures.Resources` is `map[ResourceID]interface{}` — raw sample objects
loaded from `fixtures3.json`, looked up by `XResourceID` during generation.

---

## Subsystem Deep-Dives

### Routing (`server/server.go`)

- `initializeRouter` runs once: each spec `Path` → `compilePath` → an anchored
  regex with named capture groups for `{params}`; each operation gets a `jsval`
  validator (GET uses `BuildQuerySchema`, others use the request-body schema).
- Routes per verb are **sorted by number of path params ascending**, so static
  segments win over dynamic ones.
- `routeRequest` finds the first matching route, unescapes captures, and splits
  them into a **primary ID** (the object being acted on, present only when the
  path ends in `}` or an RPC action like `/capture`, `/confirm`, `/refund` — see
  `hasPrimaryIDSuffixes`) and **secondary IDs** (parent-resource IDs).

### Request parsing pipeline (`param/`)

```
ParseParams ─► parser.ParseFormString (query + body) ─► form.Values (ordered pairs)
            └► nestedtypeassembler.AssembleParams      ─► map[string]interface{} (still strings)
```
Coercion is **decoupled**: `server.go` calls `coercer.CoerceParams(route.requestSchema, data)`
afterward to turn strings into typed values using the schema. This keeps `param`
schema-agnostic. Example: `items[0][plan]=gold` → `{"items":[{"plan":"gold"}]}`.

### Validation (`spec/validate.go`)

OpenAPI 3 isn't directly consumable by the `jsval` library, so
`getJSONSchemaForOpenAPI3Schema` translates it to draft JSON-Schema (notably
turning `nullable:true` into a `["type","null"]` union). The resulting `*jsval.JSVal`
is what rejects unknown/mistyped params per request.

### Response generation (`server/generator.go`)

`DataGenerator.Generate` → recursive `generateInternal` over the response schema:

1. `maybeDereference` resolves `$ref` → `Components.Schemas[name]`.
2. Fixture lookup by `XResourceID`; if none, `generateSyntheticFixture` builds a
   minimal object (required props only, enums/zero-values for leaves).
3. **Lists** (`object:"list"`) → `generateListResource` wraps one item in
   `{data:[…], has_more:false, object:"list", url:…}`; **search** similarly via
   `generateSearchResultResource`.
4. **`anyOf`** → `findAnyOfBranch` picks the deleted vs. non-deleted branch based
   on the request method; expansions follow `XExpansionResources` vs. `AnyOf[0]`.
5. Post-pass on the assembled object:
   - `maybeGeneratePrimaryID` mints a `randomID` for CREATE responses with no path ID.
   - `recordAndReplaceIDs` swaps generated IDs for the real IDs from the URL path;
     `distributeReplacedIDs` propagates those swaps into nested refs and URLs.
   - `datareplacer.ReplaceData` reflects POST request fields back into the response
     where name + type match (`isSameType`), so inputs echo through.

---

## Stateful PaymentIntent layer (fork extension)

> Upstream stripe-mock is stateless by design. This fork adds an **opt-in
> stateful layer for PaymentIntents** (and their Charges) for test suites that
> need to drive a lifecycle and receive webhook events deterministically. It sits
> *beside* the generic generator: non-PaymentIntent resources are untouched, and a
> PaymentIntent that was never seeded or created falls through to the generic path.

**Files:** `server/stateful.go` (session store), `server/control.go` (the
`/v1/_mock/*` control plane), `server/paymentintents.go` (lifecycle + events).

**Two interception points in `HandleRequest`:**
1. Before auth/routing — requests under `/v1/_mock/` go to `handleControlRequest`
   (seed, reset, emit, drain). These bypass strict OpenAPI validation.
2. After `validateAndCoerceRequest` — `maybeHandleStatefulRequest` serves/mutates
   stored PaymentIntents and Charges; returns `false` to fall through.

**Opt-in (default off):** `maybeHandleStatefulRequest` returns early unless the
session has opted in — via `POST /v1/_mock/config`, by seeding, or an
`X-Stripe-Mock-Stateful` header. So the mock is byte-for-byte the generic,
stateless mock for every session that doesn't ask, which keeps existing test
suites unaffected when this binary replaces upstream.

**Session store (`statefulStore`):** `map[sessionID]*sessionStore`, mutex-guarded.
Each session holds resources keyed by `x-resourceId` → id → object, a FIFO event
queue, and a `statefulEnabled` flag. The session id comes from the API key (or an
explicit `X-Stripe-Mock-Session` header), so parallel test workers stay isolated;
`reset` drops a session.

**Seeding (`generateResourceBase` + `mergeMap`):** seeds reuse the `DataGenerator`
to build a spec-correct base object, then deep-merge caller overrides — seeded
objects track the official schema with no hand-maintained fixtures.

**Lifecycle (`paymentintents.go`):** `create`/`confirm`/`capture`/`cancel`/`update`
move the object through the state machine. A `confirm`'s outcome is chosen by the
payment method via the `magicPaymentMethods` table (Stripe's documented test ids).

**Events (`recordPaymentIntentTransition`):** each transition builds a `Charge`
(when settling) and enqueues deep-copied webhook-event envelopes. Nothing is
delivered automatically — `GET /v1/_mock/events` drains FIFO on the test's
command, which is what makes event ordering and timing deterministic (no sleeps).

| Control endpoint | Handler |
|---|---|
| `POST /v1/_mock/config` | `handleConfig` (opt the session into the stateful layer) |
| `POST /v1/_mock/payment_intents` | `handleSeedPaymentIntent` (base + overrides; also opts in) |
| `POST /v1/_mock/payment_intents/{id}/emit` | `handleEmitPaymentIntentEvent` |
| `GET /v1/_mock/events` | drain via `statefulStore.drainEvents` |
| `POST /v1/_mock/reset` | `statefulStore.reset` |

See the README's [Stateful PaymentIntent testing](README.md#stateful-paymentintent-testing-fork-extension)
section for the user-facing endpoint reference and examples.

---

## Build, Test & Run

```sh
# build a self-contained binary (uses vendored deps)
make build            # → ./stripe-mock

# full local gate: test + vet + lint + gofmt + build
make all

# tests only (cache disabled)
go test ./... -count=1

# run (HTTP :12111, HTTPS :12112 by default)
./stripe-mock
./stripe-mock -http-port 0          # auto-pick a port
./stripe-mock -beta                 # use embedded beta spec/fixtures
./stripe-mock -spec ./my-spec.json -fixtures ./my-fixtures.json   # override embedded assets
./stripe-mock -strict-version-check # 400 on Stripe-Version mismatch

# sample request
curl -i http://localhost:12111/v1/charges -H "Authorization: Bearer sk_test_123"

# refresh the bundled Stripe spec + fixtures
make update-openapi-spec            # OPENAPI_BRANCH=master by default

# docker
make docker-build && make docker-run
```

Releases are produced by **goreleaser** on tag push; `version` is injected via
`-ldflags -X` (defaults to `master` from source).

---

## "Where do I look to change X?"

| I want to… | Go to |
|---|---|
| Add/adjust a CLI flag or listener option | `main.go` (`options`, flag block, `getHTTPListener`) |
| Change auth acceptance rules | `validateAuth` in `server/server.go` |
| Change how URLs are matched / add an RPC action suffix | `compilePath` + `hasPrimaryIDSuffixes` in `server/server.go` |
| Fix request param parsing (brackets, arrays) | `param/nestedtypeassembler/` |
| Fix string→type coercion | `param/coercer/coercer.go` |
| Change request validation behavior | `spec/validate.go` (+ `spec/query.go` for GET) |
| Change what the response body looks like | `server/generator.go` |
| Change how inputs are echoed into responses | `generator/datareplacer/datareplacer.go` |
| Support a new `x-…` schema field after a spec bump | `supportedSchemaFields` in `spec/spec.go`, then add to `Schema` if it drives behavior |
| Update to a newer Stripe API version | `make update-openapi-spec`, then run tests |
| Add response headers / change serialization | `writeResponse` / `HandleRequest` in `server/server.go` |
| Add/adjust a magic test card outcome | `magicPaymentMethods` in `server/paymentintents.go` |
| Change a PaymentIntent state transition or its events | `applyConfirm`/`applyCapture`/… + `recordPaymentIntentTransition` in `server/paymentintents.go` |
| Add a `/v1/_mock/*` control endpoint | `handleControlRequest` in `server/control.go` |
| Make another resource stateful (not just PaymentIntents) | `maybeHandleStatefulRequest` (`server/paymentintents.go`) + the generic store in `server/stateful.go` |

---

## Glossary

- **Fixture** — a hardcoded sample resource object (in `fixtures3.json`) returned
  for a given `x-resourceId`.
- **Primary ID** — the ID of the object an endpoint acts on and returns
  (`/v1/charges/{id}` or `/v1/charges/{id}/capture`); reflected from the URL into the response.
- **Secondary ID** — a non-primary ID in the path, usually a parent resource
  (`/v1/application_fees/{fee}/refunds`).
- **Expansion** — Stripe's `expand[]=field` mechanism that inlines a related
  object instead of just its ID; modeled by `ExpansionLevel` + `XExpansionResources`.
- **Synthetic fixture** — a minimal object the generator fabricates when a schema
  has no matching fixture.
- **Coercion** — converting string form values to typed values (int/bool/number)
  per the schema, since HTTP form data is all strings.
- **`jsval`** — the `lestrrat-go/jsval` JSON-Schema validation library used for
  request validation.
- **Session** *(fork)* — an isolation bucket for stateful resources, keyed by the
  request's API key (or `X-Stripe-Mock-Session` header), so parallel test workers
  don't share state.
- **Control plane** *(fork)* — the test-only `/v1/_mock/*` endpoints used to seed,
  reset, emit events for, and drain events from a session.
- **Seed** *(fork)* — injecting a PaymentIntent (base + overrides) into a session
  via `POST /v1/_mock/payment_intents` so later API calls operate on it.
```
