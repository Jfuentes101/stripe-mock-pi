# stripe-mock [![Build Status](https://github.com/stripe/stripe-mock/actions/workflows/ci.yml/badge.svg)](https://github.com/stripe/stripe-mock/actions/workflows/ci.yml)

stripe-mock is a mock HTTP server based on the real Stripe API. It accepts the
same requests and parameters that the Stripe API accepts, and rejects requests
whose parameters are not recognized or have incorrect types. Its responses
resemble the responses of the real Stripe API in terms of data type; however,
stripe-mock **does not attempt to reproduce the _behavior_ of the real Stripe API
at all**. It cannot reject all invalid requests, and its responses are completely
hardcoded. They will have a correct type, but they will not necessarily be
realistic Stripe responses.

stripe-mock is meant for basic sanity checks. We use it in the test suites of
our server-side SDKs, like [stripe-ruby](https://github.com/stripe/stripe-ruby),
[stripe-go](https://github.com/stripe/stripe-go), etc, to help validate that the
SDK hits the right URL and sends the right parameters. If you have more
sophisticated testing needs, you shouldn't use stripe-mock. Always test changes
to your Stripe integration against
[testmode](https://stripe.com/docs/keys#test-live-modes). For regression test
suites, you should define your own mocks, or use a playback testing tool such as
the [VCR gem](https://github.com/vcr/vcr).

While stripe-mock is naïve, it is powered by
[the Stripe OpenAPI specification][openapi] and is therefore kept up-to-date
with the latest methods, resources, and fields.

## Features and limitations

stripe-mock supports the following features:

- It has a catalog of every API URL and their signatures. It responds to URLs
  that exist with a resource that it returns and 404s on URLs that don't exist.
- JSON Schema is used to check the validity of the parameters of incoming
  requests. Validation is comprehensive, but far from exhaustive, so don't
  expect the full barrage of checks of the live API.
- Responses are generated based off resource fixtures. They're also generated
  from within Stripe's API, and similar to the sample data available in Stripe's
  [API reference][apiref]. **They are hardcoded**, and will not necessarily
  represent realistic responses based on the parameters you input into the
  request.
- It reflects the values of valid input parameters into responses where the
  naming and type are the same. So if a charge is created with `amount=123`, a
  charge will be returned with `"amount": 123`.
- It will respond over HTTP or over HTTPS. HTTP/2 over HTTPS is available if the
  client supports it.

Limitations:

- stripe-mock is stateless by default. Data you send on a `POST` request will be
  validated, but it will be completely ignored beyond that. It will not be
  reflected on the response or on any future request -- unlike the real Stripe
  API, which stores the information you send it. (This fork adds an opt-in
  stateful layer for PaymentIntents; see
  [Stateful PaymentIntent testing](#stateful-paymentintent-testing-fork-extension).)
- For polymorphic endpoints (say one that returns either a card or a bank
  account), only a single resource type is ever returned. There's no way to
  specify which one that is.
- It's locked to the latest version of Stripe's API and doesn't support old
  versions.
- [Testing for specific responses and errors](https://stripe.com/docs/testing#cards-responses)
  is currently not supported. It will return a success response instead of the
  desired error response.

## Future plans

The scope we envision for stripe-mock has significantly narrowed since 2017 when
we first released it. Back in 2017, our vision was for stripe-mock was to return
responses that were _realistic_ as well as just having the expected types. This
has changed. We are currently **not** planning to add statefulness or more
sophisticated testing features to stripe-mock. stripe-mock will remain a tool
for basic sanity checks. If you have more sophisticated needs, you should define
your own mocks, use a playback testing tool like the
[VCR gem](https://github.com/vcr/vcr), or find a community library you trust. Be
careful, though. Always test changes to your Stripe integration against
testmode. Mock implementations of Stripe can never behave exactly at the Stripe
API does, and might differ in nuanced (and potentially dangerous) ways.

## Usage

If you have Go installed, you can install the basic binary with:

```sh
go install github.com/stripe/stripe-mock@latest
```

With no arguments, stripe-mock will listen with HTTP on its default port of
`12111` and HTTPS on `12112`:

```sh
stripe-mock
```

Ports can be specified explicitly with:

```sh
stripe-mock -http-port 12111 -https-port 12112
```

(Leave either `-http-port` or `-https-port` out to activate stripe-mock on only
one protocol.)

Have stripe-mock select a port automatically by passing `0`:

```sh
stripe-mock -http-port 0
```

It can also listen via Unix socket:

```sh
stripe-mock -http-unix /tmp/stripe-mock.sock -https-unix /tmp/stripe-mock-secure.sock
```

### Homebrew

Get it from Homebrew or download it [from the releases page][releases]:

```sh
brew install stripe/stripe-mock/stripe-mock

# start a stripe-mock service at login
brew services start stripe-mock

# upgrade if you already have it
brew upgrade stripe-mock

# restart the service after upgrading
brew services restart stripe-mock
```

The Homebrew service listens on port `12111` for HTTP and `12112` for HTTPS and
HTTP/2.

### Docker

```sh
docker run --rm -it -p 12111-12112:12111-12112 stripe/stripe-mock:latest
```

The default Docker `ENTRYPOINT` listens on port `12111` for HTTP and `12112` for
HTTPS and HTTP/2.

### Sample request

After you've started stripe-mock, you can try a sample request against it:

```sh
curl -i http://localhost:12111/v1/charges -H "Authorization: Bearer sk_test_123"
```

## Stateful PaymentIntent testing (fork extension)

> This is an extension in this fork. Upstream stripe-mock is intentionally
> stateless (see [Limitations](#features-and-limitations)), and behavior for
> every resource other than PaymentIntents is unchanged. The endpoints below live
> under `/v1/_mock/` and are meant for test suites only.

For PaymentIntents, stripe-mock can keep state across requests so a test can
drive a realistic lifecycle (`requires_payment_method` → `requires_action` →
`succeeded`, the manual-capture branch, declines, etc.) and receive the matching
webhook events on demand. State is built on the spec-correct base object, so it
stays faithful to Stripe's official schemas without hand-maintained fixtures.

### Opting in

The stateful layer is **off by default** — every session behaves like the
generic, stateless mock until it opts in, so existing test suites are unaffected.
A session opts in by calling `POST /v1/_mock/config` (typically in `setup`),
seeding a PaymentIntent, or sending an `X-Stripe-Mock-Stateful` header on a
request. When off, PaymentIntent requests fall through to the normal generator.

### How state is scoped

State is partitioned per **session** so parallel test workers don't collide. The
session id is the API key from the `Authorization` header (use a distinct key per
worker), or an explicit `X-Stripe-Mock-Session` header. `POST /v1/_mock/reset`
clears the current session — call it between tests.

### Driving the lifecycle

`create`, `confirm`, `capture`, `cancel`, and `update` mutate a stored
PaymentIntent. A POST that references an id the mock has never seen (say, one
from your app's own DB fixtures, or created before the session opted in)
**adopts** it: a spec-correct base is built for that id and the lifecycle
continues from there — just like real Stripe, which would know the id.

State transitions are validated like the real API: capturing an intent that
isn't in `requires_capture` is rejected with an HTTP 400
`payment_intent_unexpected_state` error, so tests can reproduce capture races.

The outcome of a `confirm` is chosen by the payment method, using Stripe's
documented [test payment methods](https://stripe.com/docs/testing):

| Payment method | Result on confirm |
|---|---|
| `pm_card_visa` (and unknown ids) | `succeeded` (or `requires_capture` when `capture_method=manual`) |
| `pm_card_authenticationRequired` | `requires_action` (with a `next_action`) |
| `pm_card_chargeDeclined` | `requires_payment_method` (with a `last_payment_error`) |

### Events

Transitions queue Stripe webhook-event envelopes; nothing is delivered
automatically. Drain them when your test is ready — this is what makes event
ordering and timing deterministic (no sleeps):

| Transition | Events queued |
|---|---|
| create | `payment_intent.created` |
| confirm (automatic capture) → succeeded | `charge.succeeded`, `payment_intent.succeeded` |
| confirm (manual capture) → authorized | `charge.succeeded` (with `captured: false`), `payment_intent.amount_capturable_updated` |
| capture → succeeded | `charge.captured`, `payment_intent.succeeded` |
| confirm → declined | `charge.failed`, `payment_intent.payment_failed` |
| confirm → 3DS | `payment_intent.requires_action` |
| cancel | `payment_intent.canceled` |

Known limitation: multicapture (`final_capture=false`) is not simulated — every
capture is final.

Like real Stripe, a manual-capture authorization creates the charge immediately —
`charge.succeeded` fires at authorization time with `captured: false`, and the
later capture fires `charge.captured` (not a second `charge.succeeded`). This is
what lets tests reproduce capture races and books-marked-paid bugs faithfully.

### Control endpoints

| Endpoint | Purpose |
|---|---|
| `POST /v1/_mock/config` | Opt the session into the stateful layer (`{"stateful_payment_intents": true}`; defaults to true when the body is empty). |
| `POST /v1/_mock/payment_intents` | Seed a PaymentIntent. The body is a JSON object of overrides, deep-merged onto a spec-correct base (e.g. `{"id":"pi_x","amount":5530,"status":"requires_action"}`). Also opts the session in. |
| `GET /v1/_mock/payment_intents` | List the session's stored PaymentIntents — lets a test discover which intent the app created or updated, e.g. to then drive its confirm. |
| `POST /v1/_mock/payment_intents/{id}/emit?type=...` | Queue an event wrapping the current state of a stored PaymentIntent (type derived from status when omitted). |
| `GET /v1/_mock/events` | Drain queued events (FIFO) for the session. |
| `POST /v1/_mock/reset` | Clear all state for the session. |

Settled PaymentIntents get a real `Charge` (wired to `latest_charge`),
retrievable with `GET /v1/charges/{id}`.

### Example

```sh
AUTH='Authorization: Bearer sk_test_123'

# Seed a PaymentIntent exactly as the test needs it.
curl -s -X POST localhost:12111/v1/_mock/payment_intents -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"id":"pi_test","amount":5530,"currency":"usd","status":"requires_confirmation"}'

# Confirm it with a 3DS test card -> status requires_action.
curl -s -X POST localhost:12111/v1/payment_intents/pi_test/confirm -H "$AUTH" \
  -d 'payment_method=pm_card_authenticationRequired'

# Drain the queued events when ready -> payment_intent.requires_action.
curl -s localhost:12111/v1/_mock/events -H "$AUTH"

# Reset between tests.
curl -s -X POST localhost:12111/v1/_mock/reset -H "$AUTH"
```

## Development

### Testing

Run the test suite:

```sh
go test ./...
```

### Updating OpenAPI

Update the OpenAPI spec by running `make update-openapi-spec` in the root of the
repo.

```sh
make update-openapi-spec
```

## Dependencies

Dependencies are managed using [go modules][gomod] and require Go 1.11+ with
`GO111MODULE=on`.

## Release

Releases are automatically published by Travis CI using [goreleaser] when a new
tag is pushed:

```sh
git pull origin --tags
git tag v0.1.1
git push origin --tags
```

[apiref]: https://stripe.com/docs/api
[go-bindata]: https://github.com/go-bindata/go-bindata
[gomod]: https://golang.org/ref/mod
[goreleaser]: https://github.com/goreleaser/goreleaser
[openapi]: https://github.com/stripe/openapi
[releases]: https://github.com/stripe/stripe-mock/releases

<!--
# vim: set tw=79:
-->
