package server

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	assert "github.com/stretchr/testify/require"
)

// newRealStubServer builds a StubServer backed by the real embedded spec and
// fixtures (loaded once into realSpec/realFixtures by the package init). The
// control-plane tests need the genuine payment_intent schema + fixture so the
// seeded base is spec-correct.
func newRealStubServer(t *testing.T) *StubServer {
	server := &StubServer{spec: &realSpec, fixtures: &realFixtures}
	err := server.initializeRouter()
	assert.NoError(t, err)
	return server
}

// roundtrip sends a request through the full HandleRequest path and returns the
// status code and decoded JSON body.
func roundtrip(server *StubServer, method, url, body string, headers map[string]string) (int, map[string]interface{}) {
	req := httptest.NewRequest(method, "https://stripe.com"+url, bytes.NewBufferString(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Opt every test request into the stateful layer (it's off by default).
	req.Header.Set("X-Stripe-Mock-Stateful", "1")
	w := httptest.NewRecorder()
	server.HandleRequest(w, req)

	resp := w.Result()
	raw, _ := ioutil.ReadAll(resp.Body)
	var parsed map[string]interface{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &parsed)
	}
	return resp.StatusCode, parsed
}

func TestStatefulLayerIsOptIn(t *testing.T) {
	server := newRealStubServer(t)
	key := "sk_test_optout"

	// Create a PaymentIntent WITHOUT opting in: no X-Stripe-Mock-Stateful header,
	// no /config, no seed. This must fall through to the generic generator.
	req := httptest.NewRequest(http.MethodPost, "https://stripe.com/v1/payment_intents",
		bytes.NewBufferString("amount=1000&currency=usd&payment_method=pm_card_visa&confirm=true"))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	server.HandleRequest(w, req)
	assert.Equal(t, http.StatusOK, w.Result().StatusCode)

	// The stateful path would have queued payment_intent.created/succeeded; the
	// generic path queues nothing. An empty queue confirms we stayed generic.
	assert.Empty(t, drainEvents(server, key))
}

func TestControlPlanePaymentIntents(t *testing.T) {
	server := newRealStubServer(t)
	authA := map[string]string{
		"Authorization": "Bearer sk_test_alpha",
		"Content-Type":  "application/json",
	}

	t.Run("seed then retrieve returns the seeded object", func(t *testing.T) {
		status, seeded := roundtrip(server, http.MethodPost, "/v1/_mock/payment_intents",
			`{"id":"pi_seed_1","amount":873421,"status":"requires_capture"}`, authA)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "pi_seed_1", seeded["id"])
		assert.Equal(t, float64(873421), seeded["amount"])
		assert.Equal(t, "requires_capture", seeded["status"])
		// Fields we didn't override come from the spec-correct base.
		assert.Equal(t, "payment_intent", seeded["object"])

		status, got := roundtrip(server, http.MethodGet, "/v1/payment_intents/pi_seed_1", "", authA)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "pi_seed_1", got["id"])
		assert.Equal(t, float64(873421), got["amount"])
		assert.Equal(t, "requires_capture", got["status"])
	})

	t.Run("reset clears seeded state", func(t *testing.T) {
		roundtrip(server, http.MethodPost, "/v1/_mock/payment_intents",
			`{"id":"pi_seed_2","amount":873422}`, authA)

		status, _ := roundtrip(server, http.MethodPost, "/v1/_mock/reset", "", authA)
		assert.Equal(t, http.StatusOK, status)

		// After reset the retrieve falls through to the generic generator, so the
		// seeded amount is gone.
		status, got := roundtrip(server, http.MethodGet, "/v1/payment_intents/pi_seed_2", "", authA)
		assert.Equal(t, http.StatusOK, status)
		assert.NotEqual(t, float64(873422), got["amount"])
	})

	t.Run("sessions are isolated", func(t *testing.T) {
		roundtrip(server, http.MethodPost, "/v1/_mock/payment_intents",
			`{"id":"pi_iso","amount":555111}`, authA)

		authB := map[string]string{"Authorization": "Bearer sk_test_beta"}
		status, gotB := roundtrip(server, http.MethodGet, "/v1/payment_intents/pi_iso", "", authB)
		assert.Equal(t, http.StatusOK, status)
		assert.NotEqual(t, float64(555111), gotB["amount"], "session B cannot see session A's seed")

		status, gotA := roundtrip(server, http.MethodGet, "/v1/payment_intents/pi_iso", "", authA)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, float64(555111), gotA["amount"])
	})

	t.Run("unknown control endpoint 404s", func(t *testing.T) {
		status, _ := roundtrip(server, http.MethodPost, "/v1/_mock/bogus", "", authA)
		assert.Equal(t, http.StatusNotFound, status)
	})
}
