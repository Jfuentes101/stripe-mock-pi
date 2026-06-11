package server

import (
	"net/http"
	"testing"

	assert "github.com/stretchr/testify/require"
)

func drainEvents(server *StubServer, key string) []map[string]interface{} {
	_, body := roundtrip(server, http.MethodGet, "/v1/_mock/events", "",
		map[string]string{"Authorization": "Bearer " + key})
	raw, _ := body["data"].([]interface{})
	events := make([]map[string]interface{}, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]interface{}); ok {
			events = append(events, m)
		}
	}
	return events
}

func eventTypes(events []map[string]interface{}) []string {
	types := make([]string, 0, len(events))
	for _, e := range events {
		types = append(types, e["type"].(string))
	}
	return types
}

func eventObject(e map[string]interface{}) map[string]interface{} {
	data, _ := e["data"].(map[string]interface{})
	obj, _ := data["object"].(map[string]interface{})
	return obj
}

func emit(server *StubServer, url, key string) (int, map[string]interface{}) {
	return roundtrip(server, http.MethodPost, url, "", map[string]string{"Authorization": "Bearer " + key})
}

func TestPaymentIntentEvents(t *testing.T) {
	server := newRealStubServer(t)

	t.Run("create+confirm success queues created, charge.succeeded, succeeded", func(t *testing.T) {
		key := "sk_test_e1"
		postForm(server, "/v1/payment_intents",
			"amount=1000&currency=usd&payment_method=pm_card_visa&confirm=true", key)

		events := drainEvents(server, key)
		assert.Equal(t,
			[]string{"payment_intent.created", "charge.succeeded", "payment_intent.succeeded"},
			eventTypes(events))

		last := events[len(events)-1]
		assert.Equal(t, "event", last["object"])
		obj := eventObject(last)
		assert.Equal(t, "payment_intent", obj["object"])
		assert.Equal(t, "succeeded", obj["status"])

		assert.Empty(t, drainEvents(server, key), "draining again yields nothing")
	})

	t.Run("plain create queues only created (fixture junk must not look like a decline)", func(t *testing.T) {
		key := "sk_test_e0"
		_, pi := postForm(server, "/v1/payment_intents", "amount=1000&currency=usd", key)
		assert.Equal(t, "requires_payment_method", pi["status"])
		assert.Nil(t, pi["last_payment_error"], "fixture sample error must be cleared")
		assert.Nil(t, pi["next_action"], "fixture sample next_action must be cleared")

		assert.Equal(t, []string{"payment_intent.created"}, eventTypes(drainEvents(server, key)))
	})

	t.Run("decline queues charge.failed and payment_failed", func(t *testing.T) {
		key := "sk_test_e2"
		seedPI(server, `{"id":"pi_e2","status":"requires_confirmation","amount":3000,"currency":"usd"}`, key)
		postForm(server, "/v1/payment_intents/pi_e2/confirm", "payment_method=pm_card_chargeDeclined", key)

		assert.Equal(t, []string{"charge.failed", "payment_intent.payment_failed"},
			eventTypes(drainEvents(server, key)))
	})

	t.Run("3DS queues requires_action", func(t *testing.T) {
		key := "sk_test_e3"
		seedPI(server, `{"id":"pi_e3","status":"requires_confirmation","amount":2000,"currency":"usd"}`, key)
		postForm(server, "/v1/payment_intents/pi_e3/confirm", "payment_method=pm_card_authenticationRequired", key)

		assert.Equal(t, []string{"payment_intent.requires_action"},
			eventTypes(drainEvents(server, key)))
	})

	t.Run("manual capture queues amount_capturable_updated then capture events", func(t *testing.T) {
		key := "sk_test_e4"
		_, pi := postForm(server, "/v1/payment_intents",
			"amount=1000&currency=usd&payment_method=pm_card_visa&confirm=true&capture_method=manual", key)
		id := pi["id"].(string)

		// Authorization creates the (uncaptured) charge, so charge.succeeded
		// fires now — with captured=false — exactly like real Stripe.
		assert.Equal(t,
			[]string{"payment_intent.created", "charge.succeeded", "payment_intent.amount_capturable_updated"},
			eventTypes(drainEvents(server, key)))

		// Capturing the previously-authorized charge fires charge.captured (its
		// charge.succeeded already fired at authorization time).
		postForm(server, "/v1/payment_intents/"+id+"/capture", "", key)
		assert.Equal(t, []string{"charge.captured", "payment_intent.succeeded"},
			eventTypes(drainEvents(server, key)))
	})

	t.Run("charge is retrievable after success", func(t *testing.T) {
		key := "sk_test_e5"
		_, pi := postForm(server, "/v1/payment_intents",
			"amount=1234&currency=usd&payment_method=pm_card_visa&confirm=true", key)
		chargeID := pi["latest_charge"].(string)

		status, charge := getResource(server, "/v1/charges/"+chargeID, key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, chargeID, charge["id"])
		assert.Equal(t, "succeeded", charge["status"])
		assert.Equal(t, true, charge["captured"])
		assert.Equal(t, pi["id"], charge["payment_intent"])
		assert.Equal(t, float64(1234), charge["amount"])
	})

	t.Run("emit produces and queues an event for a seeded PI", func(t *testing.T) {
		key := "sk_test_e6"
		seedPI(server, `{"id":"pi_e6","status":"succeeded","amount":777}`, key)
		assert.Empty(t, drainEvents(server, key), "seeding alone queues nothing")

		status, evt := emit(server, "/v1/_mock/payment_intents/pi_e6/emit", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "payment_intent.succeeded", evt["type"], "type derived from status")
		assert.Equal(t, "pi_e6", eventObject(evt)["id"])

		assert.Equal(t, []string{"payment_intent.succeeded"}, eventTypes(drainEvents(server, key)))
	})

	t.Run("emit honors an explicit type", func(t *testing.T) {
		key := "sk_test_e7"
		seedPI(server, `{"id":"pi_e7","status":"succeeded","amount":1}`, key)
		status, evt := emit(server, "/v1/_mock/payment_intents/pi_e7/emit?type=payment_intent.canceled", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "payment_intent.canceled", evt["type"])
	})

	t.Run("reset clears queued events", func(t *testing.T) {
		key := "sk_test_e8"
		postForm(server, "/v1/payment_intents",
			"amount=10&currency=usd&payment_method=pm_card_visa&confirm=true", key)
		roundtrip(server, http.MethodPost, "/v1/_mock/reset", "",
			map[string]string{"Authorization": "Bearer " + key})
		assert.Empty(t, drainEvents(server, key))
	})

	t.Run("events are session scoped", func(t *testing.T) {
		postForm(server, "/v1/payment_intents",
			"amount=10&currency=usd&payment_method=pm_card_visa&confirm=true", "sk_test_e9a")
		assert.Empty(t, drainEvents(server, "sk_test_e9b"), "another session sees no events")
		assert.NotEmpty(t, drainEvents(server, "sk_test_e9a"))
	})
}
