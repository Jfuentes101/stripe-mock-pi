package server

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	assert "github.com/stretchr/testify/require"
)

func postForm(server *StubServer, url, body, key string) (int, map[string]interface{}) {
	return roundtrip(server, http.MethodPost, url, body, map[string]string{
		"Authorization": "Bearer " + key,
		"Content-Type":  "application/x-www-form-urlencoded",
	})
}

func getResource(server *StubServer, url, key string) (int, map[string]interface{}) {
	return roundtrip(server, http.MethodGet, url, "", map[string]string{"Authorization": "Bearer " + key})
}

func seedPI(server *StubServer, body, key string) (int, map[string]interface{}) {
	return roundtrip(server, http.MethodPost, "/v1/_mock/payment_intents", body, map[string]string{
		"Authorization": "Bearer " + key,
		"Content-Type":  "application/json",
	})
}

func TestStatefulPaymentIntentLifecycle(t *testing.T) {
	server := newRealStubServer(t)

	t.Run("create without payment method is requires_payment_method", func(t *testing.T) {
		status, pi := postForm(server, "/v1/payment_intents", "amount=1000&currency=usd", "sk_test_c1")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "requires_payment_method", pi["status"])
		assert.Equal(t, float64(1000), pi["amount"])
		assert.NotEmpty(t, pi["id"])
		assert.True(t, strings.HasPrefix(pi["client_secret"].(string), pi["id"].(string)+"_secret_"))
	})

	t.Run("create with payment method is requires_confirmation", func(t *testing.T) {
		status, pi := postForm(server, "/v1/payment_intents",
			"amount=1000&currency=usd&payment_method=pm_card_visa", "sk_test_c2")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "requires_confirmation", pi["status"])
		assert.Equal(t, "pm_card_visa", pi["payment_method"])
	})

	t.Run("create with confirm and a good card succeeds", func(t *testing.T) {
		status, pi := postForm(server, "/v1/payment_intents",
			"amount=1000&currency=usd&payment_method=pm_card_visa&confirm=true", "sk_test_c3")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "succeeded", pi["status"])
		assert.Equal(t, float64(1000), pi["amount_received"])
		assert.True(t, strings.HasPrefix(pi["latest_charge"].(string), "ch_"))
	})

	t.Run("manual capture flow: confirm -> requires_capture -> capture -> succeeded", func(t *testing.T) {
		key := "sk_test_c4"
		_, pi := postForm(server, "/v1/payment_intents",
			"amount=1000&currency=usd&payment_method=pm_card_visa&confirm=true&capture_method=manual", key)
		assert.Equal(t, "requires_capture", pi["status"])
		assert.Equal(t, float64(1000), pi["amount_capturable"], "authorized amount is capturable")
		id := pi["id"].(string)

		status, captured := postForm(server, "/v1/payment_intents/"+id+"/capture", "", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "succeeded", captured["status"])
		assert.Equal(t, float64(1000), captured["amount_received"])
		assert.Equal(t, float64(0), captured["amount_capturable"], "nothing left to capture")
	})

	t.Run("partial capture is reflected on the intent and its charge", func(t *testing.T) {
		key := "sk_test_c10"
		_, pi := postForm(server, "/v1/payment_intents",
			"amount=1000&currency=usd&payment_method=pm_card_visa&confirm=true&capture_method=manual", key)
		id := pi["id"].(string)

		_, captured := postForm(server, "/v1/payment_intents/"+id+"/capture", "amount_to_capture=600", key)
		assert.Equal(t, "succeeded", captured["status"])
		assert.Equal(t, float64(600), captured["amount_received"])

		_, charge := getResource(server, "/v1/charges/"+captured["latest_charge"].(string), key)
		assert.Equal(t, float64(1000), charge["amount"], "charge keeps the authorized amount")
		assert.Equal(t, float64(600), charge["amount_captured"], "charge reports the captured amount")
	})

	t.Run("confirm with 3DS card requires action", func(t *testing.T) {
		key := "sk_test_c5"
		seedPI(server, `{"id":"pi_3ds","status":"requires_confirmation","amount":2000}`, key)

		status, pi := postForm(server, "/v1/payment_intents/pi_3ds/confirm",
			"payment_method=pm_card_authenticationRequired", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "requires_action", pi["status"])
		assert.NotNil(t, pi["next_action"])

		// Completing the action with a good card settles it.
		status, pi = postForm(server, "/v1/payment_intents/pi_3ds/confirm",
			"payment_method=pm_card_visa", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "succeeded", pi["status"])
		assert.Nil(t, pi["next_action"])
	})

	t.Run("confirm with a declined card fails with last_payment_error", func(t *testing.T) {
		key := "sk_test_c6"
		seedPI(server, `{"id":"pi_decl","status":"requires_confirmation","amount":3000}`, key)

		status, pi := postForm(server, "/v1/payment_intents/pi_decl/confirm",
			"payment_method=pm_card_chargeDeclined", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "requires_payment_method", pi["status"])
		lpe, ok := pi["last_payment_error"].(map[string]interface{})
		assert.True(t, ok, "last_payment_error should be set")
		assert.Equal(t, "card_declined", lpe["code"])
	})

	t.Run("capture on a non-authorized intent is rejected like real Stripe", func(t *testing.T) {
		key := "sk_test_c9"
		seedPI(server, `{"id":"pi_slow","status":"requires_payment_method","amount":42899}`, key)

		status, body := postForm(server, "/v1/payment_intents/pi_slow/capture", "", key)
		assert.Equal(t, http.StatusBadRequest, status)
		errInfo := body["error"].(map[string]interface{})
		assert.Equal(t, "invalid_request_error", errInfo["type"])
		assert.Equal(t, "payment_intent_unexpected_state", errInfo["code"])
		assert.Contains(t, errInfo["message"].(string), "status of requires_payment_method")

		// The intent must be left untouched.
		_, pi := getResource(server, "/v1/payment_intents/pi_slow", key)
		assert.Equal(t, "requires_payment_method", pi["status"])
	})

	t.Run("confirm without a payment method is rejected like real Stripe", func(t *testing.T) {
		key := "sk_test_c13"
		seedPI(server, `{"id":"pi_nopm","status":"requires_payment_method","payment_method":null}`, key)

		status, body := postForm(server, "/v1/payment_intents/pi_nopm/confirm", "", key)
		assert.Equal(t, http.StatusBadRequest, status)
		errInfo := body["error"].(map[string]interface{})
		assert.Contains(t, errInfo["message"].(string), "missing a payment method")

		status, _ = postForm(server, "/v1/payment_intents", "amount=100&currency=usd&confirm=true", key)
		assert.Equal(t, http.StatusBadRequest, status, "create+confirm without a payment method must fail too")

		// Inline payment_method_data is a documented alternative and must pass,
		// attaching a freshly created payment method.
		status, pi := postForm(server, "/v1/payment_intents",
			"amount=100&currency=usd&confirm=true&payment_method_data[type]=cashapp", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "succeeded", pi["status"])
		assert.True(t, strings.HasPrefix(pi["payment_method"].(string), "pm_"),
			"inline data must attach a payment method")

		// Without confirm, inline data still attaches: the intent is confirmable.
		status, pi = postForm(server, "/v1/payment_intents",
			"amount=100&currency=usd&payment_method_data[type]=cashapp", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "requires_confirmation", pi["status"])
		status, pi = postForm(server, "/v1/payment_intents/"+pi["id"].(string)+"/confirm", "", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "succeeded", pi["status"])
	})

	t.Run("expand latest_charge returns the charge object without touching the store", func(t *testing.T) {
		key := "sk_test_c17"
		_, pi := postForm(server, "/v1/payment_intents",
			"amount=1000&currency=usd&payment_method=pm_card_visa&confirm=true&expand[]=latest_charge", key)
		charge, ok := pi["latest_charge"].(map[string]interface{})
		assert.True(t, ok, "latest_charge must be expanded to the charge object")
		assert.Equal(t, "succeeded", charge["status"])
		id := pi["id"].(string)

		_, got := getResource(server, "/v1/payment_intents/"+id+"?expand[]=latest_charge", key)
		_, ok = got["latest_charge"].(map[string]interface{})
		assert.True(t, ok, "retrieve honors the expansion")

		_, plain := getResource(server, "/v1/payment_intents/"+id, key)
		_, ok = plain["latest_charge"].(string)
		assert.True(t, ok, "the stored object keeps the id string")
	})

	t.Run("retry after decline gets a fresh charge", func(t *testing.T) {
		key := "sk_test_c16"
		seedPI(server, `{"id":"pi_retry","status":"requires_confirmation","amount":3000,"currency":"usd"}`, key)

		postForm(server, "/v1/payment_intents/pi_retry/confirm", "payment_method=pm_card_chargeDeclined", key)
		_, pi := getResource(server, "/v1/payment_intents/pi_retry", key)
		failedCharge := pi["latest_charge"].(string)

		_, pi = postForm(server, "/v1/payment_intents/pi_retry/confirm", "payment_method=pm_card_visa", key)
		assert.Equal(t, "succeeded", pi["status"])
		assert.NotEqual(t, failedCharge, pi["latest_charge"], "each attempt gets its own charge")

		// The failed attempt's charge is preserved untouched.
		_, old := getResource(server, "/v1/charges/"+failedCharge, key)
		assert.Equal(t, "failed", old["status"])
	})

	t.Run("terminal intents reject confirm and cancel", func(t *testing.T) {
		key := "sk_test_c14"
		seedPI(server, `{"id":"pi_done","status":"succeeded","payment_method":"pm_card_visa"}`, key)

		status, body := postForm(server, "/v1/payment_intents/pi_done/confirm", "payment_method=pm_card_visa", key)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Contains(t, body["error"].(map[string]interface{})["message"].(string), "status of succeeded")

		status, body = postForm(server, "/v1/payment_intents/pi_done/cancel", "", key)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Contains(t, body["error"].(map[string]interface{})["message"].(string), "status of succeeded")

		// No duplicate webhooks were queued by the rejected calls.
		assert.Empty(t, drainEvents(server, key))
	})

	t.Run("over-capture is rejected", func(t *testing.T) {
		key := "sk_test_c15"
		_, pi := postForm(server, "/v1/payment_intents",
			"amount=1000&currency=usd&payment_method=pm_card_visa&confirm=true&capture_method=manual", key)
		id := pi["id"].(string)

		status, body := postForm(server, "/v1/payment_intents/"+id+"/capture", "amount_to_capture=1500", key)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Contains(t, body["error"].(map[string]interface{})["message"].(string), "amount_to_capture")

		// A valid partial capture still works afterwards.
		status, captured := postForm(server, "/v1/payment_intents/"+id+"/capture", "amount_to_capture=600", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, float64(600), captured["amount_received"])
	})

	t.Run("concurrent captures: exactly one wins", func(t *testing.T) {
		key := "sk_test_c11"
		_, pi := postForm(server, "/v1/payment_intents",
			"amount=1000&currency=usd&payment_method=pm_card_visa&confirm=true&capture_method=manual", key)
		id := pi["id"].(string)

		results := make(chan int, 2)
		for i := 0; i < 2; i++ {
			go func() {
				status, _ := postForm(server, "/v1/payment_intents/"+id+"/capture", "", key)
				results <- status
			}()
		}
		got := []int{<-results, <-results}
		sort.Ints(got)
		assert.Equal(t, []int{http.StatusOK, http.StatusBadRequest}, got,
			"second capture must be rejected, not settled twice")
	})

	t.Run("unsimulated action endpoints fall through to the generic generator", func(t *testing.T) {
		key := "sk_test_c12"
		seedPI(server, `{"id":"pi_inc","status":"requires_capture","amount":1000}`, key)

		status, _ := postForm(server, "/v1/payment_intents/pi_inc/increment_authorization", "amount=1500", key)
		assert.Equal(t, http.StatusOK, status)

		// The stored intent must be untouched — neither updated nor re-created.
		_, pi := getResource(server, "/v1/payment_intents/pi_inc", key)
		assert.Equal(t, float64(1000), pi["amount"])
		assert.Equal(t, "requires_capture", pi["status"])
	})

	t.Run("cancel moves to canceled and releases the capturable amount", func(t *testing.T) {
		key := "sk_test_c7"
		_, pi := postForm(server, "/v1/payment_intents",
			"amount=4000&currency=usd&payment_method=pm_card_visa&confirm=true&capture_method=manual", key)
		id := pi["id"].(string)
		assert.Equal(t, float64(4000), pi["amount_capturable"])

		status, canceled := postForm(server, "/v1/payment_intents/"+id+"/cancel", "", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "canceled", canceled["status"])
		assert.Equal(t, float64(0), canceled["amount_capturable"], "authorization released")
	})

	t.Run("POST to an unknown id adopts it (update then confirm settles)", func(t *testing.T) {
		key := "sk_test_adopt"
		// Mirrors the app flow: it updates a PaymentIntent whose id comes from
		// its own DB (never seen by the mock), then the browser-side confirm is
		// simulated with a confirm call.
		status, pi := postForm(server, "/v1/payment_intents/pi_from_db_fixture",
			"amount=28030&currency=usd&payment_method=card_saved_123", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "pi_from_db_fixture", pi["id"])
		assert.Equal(t, float64(28030), pi["amount"])
		assert.Equal(t, "requires_confirmation", pi["status"])

		status, pi = postForm(server, "/v1/payment_intents/pi_from_db_fixture/confirm", "", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "succeeded", pi["status"])
		assert.Equal(t, float64(28030), pi["amount_received"])

		// The settled charge carries the adopted PaymentIntent's data.
		chargeID := pi["latest_charge"].(string)
		_, charge := getResource(server, "/v1/charges/"+chargeID, key)
		assert.Equal(t, "pi_from_db_fixture", charge["payment_intent"])
		assert.Equal(t, float64(28030), charge["amount"])
	})

	t.Run("control plane lists the session's PaymentIntents", func(t *testing.T) {
		key := "sk_test_list"
		seedPI(server, `{"id":"pi_l1","amount":1}`, key)
		seedPI(server, `{"id":"pi_l2","amount":2}`, key)

		_, body := getResource(server, "/v1/_mock/payment_intents", key)
		data := body["data"].([]interface{})
		assert.Len(t, data, 2)
		ids := []string{
			data[0].(map[string]interface{})["id"].(string),
			data[1].(map[string]interface{})["id"].(string),
		}
		assert.Equal(t, []string{"pi_l1", "pi_l2"}, ids)
	})

	t.Run("bare POST updates without changing status", func(t *testing.T) {
		key := "sk_test_c8"
		seedPI(server, `{"id":"pi_upd","status":"requires_confirmation","amount":5000}`, key)

		status, pi := postForm(server, "/v1/payment_intents/pi_upd", "description=updated+desc", key)
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "updated desc", pi["description"])
		assert.Equal(t, "requires_confirmation", pi["status"], "update must not change status")

		// And the change persists on retrieve.
		_, got := getResource(server, "/v1/payment_intents/pi_upd", key)
		assert.Equal(t, "updated desc", got["description"])
	})
}
