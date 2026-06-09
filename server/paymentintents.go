package server

import (
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
func (s *StubServer) maybeHandleStatefulPaymentIntent(w http.ResponseWriter, r *http.Request,
	start time.Time, route *stubServerRoute, pathParams *PathParamsMap,
	requestData map[string]interface{}) bool {

	if s.routeResourceID(route) != paymentIntentResourceID {
		return false
	}
	session := sessionID(r)

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
		case "capture":
			applyCapture(pi, requestData)
		case "cancel":
			applyCancel(pi, requestData)
		default:
			applyUpdate(pi, requestData)
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
	ensureLatestCharge(pi)
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
	ensureLatestCharge(pi)
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

// ensureLatestCharge assigns a charge id to a settled PaymentIntent if it doesn't
// have one. A full Charge object is modeled in a later phase.
func ensureLatestCharge(pi map[string]interface{}) {
	if getString(pi, "latest_charge") == "" {
		pi["latest_charge"] = randomID("ch")
	}
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
