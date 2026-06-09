package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

//
// Stateful PaymentIntent lifecycle. This is the behavior that upstream
// stripe-mock deliberately omits: create/confirm/capture/cancel/update mutate a
// stored object and move it through the PaymentIntent state machine, so a test
// can drive a realistic flow instead of getting the same hardcoded fixture back
// every time.
//
// Which terminal state a confirm reaches is chosen by the payment method, using
// Stripe's well-known test payment-method ids (e.g. pm_card_chargeDeclined). A
// test that needs a precise edge case can instead seed the object directly via
// the /v1/_mock control plane.
//

// pmOutcome is the result a confirm drives, selected by the payment-method id.
type pmOutcome int

const (
	outcomeSucceed pmOutcome = iota
	outcomeRequiresAction
	outcomeDecline
)

// magicPaymentMethods maps Stripe's documented test payment-method ids to the
// outcome stripe-mock simulates on confirm. Anything not listed succeeds.
var magicPaymentMethods = map[string]pmOutcome{
	"pm_card_visa":                            outcomeSucceed,
	"pm_card_mastercard":                      outcomeSucceed,
	"pm_card_authenticationRequired":          outcomeRequiresAction,
	"pm_card_authenticationRequiredOnSetup":   outcomeRequiresAction,
	"pm_card_chargeDeclined":                  outcomeDecline,
	"pm_card_chargeDeclinedInsufficientFunds": outcomeDecline,
	"pm_card_chargeDeclinedLostCard":          outcomeDecline,
	"pm_card_chargeDeclinedExpiredCard":       outcomeDecline,
	"pm_card_visa_chargeDeclined":             outcomeDecline,
}

func outcomeForPaymentMethod(paymentMethod string) pmOutcome {
	if outcome, ok := magicPaymentMethods[paymentMethod]; ok {
		return outcome
	}
	return outcomeSucceed
}

// paymentIntentActions are the RPC-style suffixes we treat as state transitions.
// A POST to a PaymentIntent path without one of these is an update.
var paymentIntentActions = []string{"confirm", "capture", "cancel"}

// maybeHandleStatefulPaymentIntent intercepts requests targeting a PaymentIntent
// and serves them from the session store when appropriate. It returns true if it
// handled (and wrote) the response; false means the caller should fall through to
// the generic spec-driven generator.
//
// It runs after request validation, so requestData is parsed, coerced, and valid.
func (s *StubServer) maybeHandleStatefulRequest(w http.ResponseWriter, r *http.Request,
	start time.Time, route *stubServerRoute, pathParams *PathParamsMap,
	requestData map[string]interface{}) bool {

	resourceID := s.routeResourceID(route)
	session := sessionID(r)

	// Charges are created as a side effect of PaymentIntent transitions; we only
	// serve retrieves of them from the store. Everything else uses the generic
	// generator.
	if resourceID == chargeResourceID {
		if r.Method == http.MethodGet && pathParams != nil && pathParams.PrimaryID != nil {
			if charge, ok := s.store.getCharge(session, *pathParams.PrimaryID); ok {
				writeResponse(w, r, start, http.StatusOK, charge)
				return true
			}
		}
		return false
	}

	if resourceID != paymentIntentResourceID {
		return false
	}

	switch {
	// Retrieve: serve a stored object if we have one.
	case r.Method == http.MethodGet:
		if pathParams == nil || pathParams.PrimaryID == nil {
			return false // list endpoint — leave to the generic generator
		}
		pi, ok := s.store.getPaymentIntent(session, *pathParams.PrimaryID)
		if !ok {
			return false
		}
		writeResponse(w, r, start, http.StatusOK, pi)
		return true

	// Create: a POST with no primary id in the path.
	case r.Method == http.MethodPost && (pathParams == nil || pathParams.PrimaryID == nil):
		pi, err := s.createPaymentIntent(session, requestData)
		if err != nil {
			fmt.Printf("Couldn't create stateful PaymentIntent: %v\n", err)
			writeResponse(w, r, start, http.StatusInternalServerError, createInternalServerError())
			return true
		}
		writeResponse(w, r, start, http.StatusOK, pi)
		return true

	// Action or update on an existing PaymentIntent.
	case r.Method == http.MethodPost:
		id := *pathParams.PrimaryID
		pi, ok := s.store.getPaymentIntent(session, id)
		if !ok {
			return false // not seeded/created here — leave to the generic generator
		}

		switch paymentIntentActionFromPath(r.URL.Path) {
		case "confirm":
			applyConfirm(pi, requestData)
			s.recordPaymentIntentTransition(session, pi, false)
		case "capture":
			applyCapture(pi, requestData)
			s.recordPaymentIntentTransition(session, pi, false)
		case "cancel":
			applyCancel(pi, requestData)
			s.recordPaymentIntentTransition(session, pi, false)
		default:
			applyUpdate(pi, requestData) // a plain update emits no event
		}

		s.store.putPaymentIntent(session, id, pi)
		writeResponse(w, r, start, http.StatusOK, pi)
		return true
	}

	return false
}

// createPaymentIntent builds a new PaymentIntent from request params on top of a
// spec-correct base, assigns an id and client_secret, sets the initial status,
// stores it, and returns it.
func (s *StubServer) createPaymentIntent(session string, params map[string]interface{}) (map[string]interface{}, error) {
	pi, err := s.generateResourceBase(paymentIntentResourceID)
	if err != nil {
		return nil, err
	}

	copyPaymentIntentParams(pi, params)

	id := getString(pi, "id")
	if id == "" {
		id = randomID("pi")
		pi["id"] = id
	}
	pi["client_secret"] = id + "_secret_" + randomIDRandomPart()

	switch {
	case boolParam(params, "confirm"):
		// confirm=true on create runs the same transition as a confirm call.
		applyConfirm(pi, params)
	case getString(pi, "payment_method") != "":
		setStatus(pi, "requires_confirmation")
	default:
		setStatus(pi, "requires_payment_method")
	}

	s.recordPaymentIntentTransition(session, pi, true)
	s.store.putPaymentIntent(session, id, pi)
	return pi, nil
}

// applyConfirm moves a PaymentIntent forward on confirm, choosing the outcome
// from its payment method (overridable by a payment_method param).
func applyConfirm(pi, params map[string]interface{}) {
	if pm := getString(params, "payment_method"); pm != "" {
		pi["payment_method"] = pm
	}

	switch outcomeForPaymentMethod(getString(pi, "payment_method")) {
	case outcomeRequiresAction:
		setStatus(pi, "requires_action")
		pi["last_payment_error"] = nil
		pi["next_action"] = map[string]interface{}{"type": "use_stripe_sdk"}
	case outcomeDecline:
		setStatus(pi, "requires_payment_method")
		pi["next_action"] = nil
		pi["last_payment_error"] = declineError()
	default:
		settlePaymentIntent(pi, params)
	}
}

// settlePaymentIntent marks a confirmed PaymentIntent succeeded, or requires_capture
// when manual capture was requested.
func settlePaymentIntent(pi, params map[string]interface{}) {
	pi["next_action"] = nil
	pi["last_payment_error"] = nil

	captureMethod := getString(pi, "capture_method")
	if cm := getString(params, "capture_method"); cm != "" {
		captureMethod = cm
	}
	if captureMethod == "manual" {
		setStatus(pi, "requires_capture")
		return
	}

	setStatus(pi, "succeeded")
	pi["amount_received"] = pi["amount"]
}

// applyCapture captures a requires_capture PaymentIntent, settling it to succeeded.
func applyCapture(pi, params map[string]interface{}) {
	setStatus(pi, "succeeded")
	pi["next_action"] = nil
	if amount, ok := params["amount_to_capture"]; ok {
		pi["amount_received"] = amount
	} else {
		pi["amount_received"] = pi["amount"]
	}
}

// applyCancel cancels a PaymentIntent.
func applyCancel(pi, params map[string]interface{}) {
	setStatus(pi, "canceled")
	pi["next_action"] = nil
	if reason := getString(params, "cancellation_reason"); reason != "" {
		pi["cancellation_reason"] = reason
	}
}

// applyUpdate merges a known set of mutable params into a stored PaymentIntent.
func applyUpdate(pi, params map[string]interface{}) {
	copyPaymentIntentParams(pi, params)
}

//
// Helpers
//

// paymentIntentParamKeys is the set of request params we reflect into a stored
// PaymentIntent on create/update. Status and lifecycle fields are owned by the
// transition logic, not copied from the request.
var paymentIntentParamKeys = []string{
	"amount",
	"application_fee_amount",
	"capture_method",
	"confirmation_method",
	"currency",
	"customer",
	"description",
	"metadata",
	"on_behalf_of",
	"payment_method",
	"payment_method_types",
	"receipt_email",
	"setup_future_usage",
	"statement_descriptor",
	"transfer_group",
}

func copyPaymentIntentParams(pi, params map[string]interface{}) {
	if params == nil {
		return
	}
	for _, key := range paymentIntentParamKeys {
		if val, ok := params[key]; ok {
			pi[key] = val
		}
	}
}

// paymentIntentActionFromPath returns the RPC action suffix of a PaymentIntent
// path (confirm/capture/cancel), or "update" for a bare POST to the object.
func paymentIntentActionFromPath(path string) string {
	for _, action := range paymentIntentActions {
		if strings.HasSuffix(path, "/"+action) {
			return action
		}
	}
	return "update"
}

func setStatus(pi map[string]interface{}, status string) {
	pi["status"] = status
}

//
// Events + Charge model
//
// Transitions enqueue webhook-event envelopes (and create Charge objects) into
// the session store. Nothing is delivered automatically — the test drains the
// queue via GET /v1/_mock/events when it's ready, which is what makes event
// timing deterministic.

// recordPaymentIntentTransition enqueues the events (and builds the Charge) that
// correspond to a PaymentIntent's current status. isCreate adds a
// payment_intent.created event ahead of any settlement events.
func (s *StubServer) recordPaymentIntentTransition(session string, pi map[string]interface{}, isCreate bool) {
	if isCreate {
		s.enqueuePIEvent(session, pi, "payment_intent.created")
	}

	switch getString(pi, "status") {
	case "succeeded":
		charge := s.ensureChargeForPI(session, pi, "succeeded", true)
		s.enqueueChargeEvent(session, charge, "charge.succeeded")
		s.enqueuePIEvent(session, pi, "payment_intent.succeeded")
	case "requires_capture":
		// Funds are authorized but not captured yet: a charge exists, uncaptured.
		s.ensureChargeForPI(session, pi, "succeeded", false)
		s.enqueuePIEvent(session, pi, "payment_intent.amount_capturable_updated")
	case "requires_action":
		s.enqueuePIEvent(session, pi, "payment_intent.requires_action")
	case "processing":
		s.enqueuePIEvent(session, pi, "payment_intent.processing")
	case "requires_payment_method":
		// Only a confirm-time decline (which sets last_payment_error) is a
		// failure; the same status at creation is just the initial state.
		if pi["last_payment_error"] != nil {
			charge := s.ensureChargeForPI(session, pi, "failed", false)
			s.enqueueChargeEvent(session, charge, "charge.failed")
			s.enqueuePIEvent(session, pi, "payment_intent.payment_failed")
		}
	case "canceled":
		s.enqueuePIEvent(session, pi, "payment_intent.canceled")
	}
}

// ensureChargeForPI creates or updates the Charge backing a PaymentIntent,
// reusing the PI's latest_charge id when present so capture mutates the same
// object. The Charge is built on a spec-correct base and stored in the session.
func (s *StubServer) ensureChargeForPI(session string, pi map[string]interface{}, status string, captured bool) map[string]interface{} {
	id := getString(pi, "latest_charge")

	var charge map[string]interface{}
	if id != "" {
		if existing, ok := s.store.getCharge(session, id); ok {
			charge = existing
		}
	}
	if charge == nil {
		base, err := s.generateResourceBase(chargeResourceID)
		if err != nil || base == nil {
			base = map[string]interface{}{}
		}
		charge = base
		if id == "" {
			id = randomID("ch")
		}
		charge["id"] = id
		pi["latest_charge"] = id
	}

	charge["object"] = "charge"
	charge["amount"] = pi["amount"]
	charge["currency"] = pi["currency"]
	charge["payment_intent"] = getString(pi, "id")
	if pm := pi["payment_method"]; pm != nil {
		charge["payment_method"] = pm
	}
	charge["status"] = status
	charge["captured"] = captured
	charge["paid"] = status == "succeeded"
	if captured {
		charge["amount_captured"] = pi["amount"]
	} else {
		charge["amount_captured"] = 0
	}

	s.store.putCharge(session, id, charge)
	return charge
}

func (s *StubServer) enqueuePIEvent(session string, pi map[string]interface{}, eventType string) {
	s.store.enqueueEvent(session, s.buildEvent(eventType, pi))
}

func (s *StubServer) enqueueChargeEvent(session string, charge map[string]interface{}, eventType string) {
	s.store.enqueueEvent(session, s.buildEvent(eventType, charge))
}

// buildEvent wraps an object in a Stripe webhook-event envelope. The object is
// deep-copied so the event captures the state at emit time, not whatever the
// stored object mutates into later.
func (s *StubServer) buildEvent(eventType string, object map[string]interface{}) map[string]interface{} {
	apiVersion := ""
	if s.spec != nil && s.spec.Info != nil {
		apiVersion = s.spec.Info.Version
	}
	return map[string]interface{}{
		"id":               randomID("evt"),
		"object":           "event",
		"api_version":      apiVersion,
		"created":          time.Now().Unix(),
		"livemode":         false,
		"pending_webhooks": 0,
		"type":             eventType,
		"request": map[string]interface{}{
			"id":              nil,
			"idempotency_key": nil,
		},
		"data": map[string]interface{}{
			"object": deepCopyMap(object),
		},
	}
}

// deepCopyMap returns a JSON-faithful deep copy of a map.
func deepCopyMap(m map[string]interface{}) map[string]interface{} {
	raw, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return m
	}
	return out
}

// paymentIntentStatusEvents maps a PaymentIntent status to the event type emitted
// when /v1/_mock/payment_intents/:id/emit is called without an explicit type.
var paymentIntentStatusEvents = map[string]string{
	"succeeded":               "payment_intent.succeeded",
	"requires_action":         "payment_intent.requires_action",
	"requires_payment_method": "payment_intent.payment_failed",
	"requires_capture":        "payment_intent.amount_capturable_updated",
	"requires_confirmation":   "payment_intent.created",
	"processing":              "payment_intent.processing",
	"canceled":                "payment_intent.canceled",
}

func eventTypeForPaymentIntentStatus(status string) string {
	if eventType, ok := paymentIntentStatusEvents[status]; ok {
		return eventType
	}
	return "payment_intent.created"
}

func declineError() map[string]interface{} {
	return map[string]interface{}{
		"type":         "card_error",
		"code":         "card_declined",
		"decline_code": "generic_decline",
		"message":      "Your card was declined.",
	}
}

func getString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func boolParam(params map[string]interface{}, key string) bool {
	if params == nil {
		return false
	}
	switch v := params[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}
