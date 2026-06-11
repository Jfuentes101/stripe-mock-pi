package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

//
// The test-control plane. These endpoints live under `/v1/_mock/` and exist only
// to let a test seed and reset stateful resources. They are handled before auth
// and routing, and they deliberately bypass the strict OpenAPI validation that
// real routes enforce — a test must be able to construct any object shape it
// needs, including edge cases.
//

// mockControlPrefix namespaces every test-control endpoint.
const mockControlPrefix = "/v1/_mock/"

// isControlRequest reports whether a request targets the test-control plane.
func isControlRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, mockControlPrefix)
}

// handleControlRequest routes a `/v1/_mock/*` request.
func (s *StubServer) handleControlRequest(w http.ResponseWriter, r *http.Request, start time.Time) {
	session := sessionID(r)
	endpoint := strings.TrimPrefix(r.URL.Path, mockControlPrefix)

	switch {
	case endpoint == "reset" && r.Method == http.MethodPost:
		s.store.reset(session)
		writeResponse(w, r, start, http.StatusOK, map[string]interface{}{"object": "_mock.reset"})

	case endpoint == "config" && r.Method == http.MethodPost:
		s.handleConfig(w, r, start, session)

	case endpoint == "payment_intents" && r.Method == http.MethodPost:
		s.handleSeedPaymentIntent(w, r, start, session)

	case endpoint == "payment_intents" && r.Method == http.MethodGet:
		// List the session's stored PaymentIntents. Lets a test discover which
		// PaymentIntent the app touched (created or adopted) so it can drive
		// the next transition (e.g. simulate the browser-side confirm).
		writeResponse(w, r, start, http.StatusOK, map[string]interface{}{
			"object": "list",
			"data":   s.store.listResources(session, paymentIntentResourceID),
		})

	case endpoint == "events" && r.Method == http.MethodGet:
		// Drain queued events in FIFO order. This is the test's deterministic
		// "deliver now" knob — events accumulate as the app drives the API and
		// are handed over only when the test asks.
		events := s.store.drainEvents(session)
		writeResponse(w, r, start, http.StatusOK, map[string]interface{}{
			"object": "list",
			"data":   events,
		})

	case r.Method == http.MethodPost && isPaymentIntentEmitEndpoint(endpoint):
		s.handleEmitPaymentIntentEvent(w, r, start, session, paymentIntentEmitID(endpoint))

	default:
		message := fmt.Sprintf("Unknown mock control endpoint (%s: %s)", r.Method, r.URL.Path)
		writeResponse(w, r, start, http.StatusNotFound,
			createStripeError(typeInvalidRequestError, message))
	}
}

// handleConfig sets per-session behavior. Currently it opts the session into the
// stateful PaymentIntent layer (default true when the body omits the flag), so
// the mock stays generic/stateless for every session that doesn't ask.
func (s *StubServer) handleConfig(w http.ResponseWriter, r *http.Request, start time.Time, session string) {
	cfg, err := decodeJSONObject(r.Body)
	if err != nil {
		message := fmt.Sprintf("Couldn't parse config body: %v", err)
		writeResponse(w, r, start, http.StatusBadRequest,
			createStripeError(typeInvalidRequestError, message))
		return
	}

	enabled := true
	if v, ok := cfg["stateful_payment_intents"].(bool); ok {
		enabled = v
	}
	s.store.setStatefulEnabled(session, enabled)

	writeResponse(w, r, start, http.StatusOK, map[string]interface{}{
		"object":                   "_mock.config",
		"stateful_payment_intents": enabled,
	})
}

// handleSeedPaymentIntent seeds a PaymentIntent into the session store. The body
// is an optional JSON object of overrides; it's deep-merged onto a spec-correct
// base generated from the OpenAPI spec + fixtures. The stored object is returned.
func (s *StubServer) handleSeedPaymentIntent(w http.ResponseWriter, r *http.Request, start time.Time, session string) {
	overrides, err := decodeJSONObject(r.Body)
	if err != nil {
		message := fmt.Sprintf("Couldn't parse seed body: %v", err)
		writeResponse(w, r, start, http.StatusBadRequest,
			createStripeError(typeInvalidRequestError, message))
		return
	}

	base, err := s.generateResourceBase(paymentIntentResourceID)
	if err != nil {
		fmt.Printf("Couldn't generate PaymentIntent base: %v\n", err)
		writeResponse(w, r, start, http.StatusInternalServerError, createInternalServerError())
		return
	}

	obj := mergeMap(base, overrides)

	// A seeded object must have an id so it can be retrieved. Honor an id from
	// the base or overrides; otherwise mint a realistic one.
	id, _ := obj["id"].(string)
	if id == "" {
		id = randomID("pi")
		obj["id"] = id
	}

	s.store.putPaymentIntent(session, id, obj)
	// Seeding a PaymentIntent implies the test wants stateful behavior.
	s.store.setStatefulEnabled(session, true)
	writeResponse(w, r, start, http.StatusOK, obj)
}

// isPaymentIntentEmitEndpoint reports whether a control endpoint path is of the
// form "payment_intents/{id}/emit".
func isPaymentIntentEmitEndpoint(endpoint string) bool {
	parts := strings.Split(endpoint, "/")
	return len(parts) == 3 && parts[0] == "payment_intents" && parts[2] == "emit"
}

// paymentIntentEmitID extracts {id} from "payment_intents/{id}/emit".
func paymentIntentEmitID(endpoint string) string {
	parts := strings.Split(endpoint, "/")
	if len(parts) == 3 {
		return parts[1]
	}
	return ""
}

// handleEmitPaymentIntentEvent enqueues a webhook event wrapping the current
// state of a stored PaymentIntent. The event type comes from the `type` query
// param, or is derived from the PaymentIntent's status when omitted. This lets a
// test seed an exact object and then emit the matching event on demand.
func (s *StubServer) handleEmitPaymentIntentEvent(w http.ResponseWriter, r *http.Request, start time.Time, session, id string) {
	pi, ok := s.store.getPaymentIntent(session, id)
	if !ok {
		message := fmt.Sprintf("No seeded PaymentIntent %q in this session to emit an event for", id)
		writeResponse(w, r, start, http.StatusNotFound,
			createStripeError(typeInvalidRequestError, message))
		return
	}

	eventType := r.URL.Query().Get("type")
	if eventType == "" {
		eventType = eventTypeForPaymentIntentStatus(getString(pi, "status"))
	}

	event := s.buildEvent(eventType, pi)
	s.store.enqueueEvent(session, event)
	writeResponse(w, r, start, http.StatusOK, event)
}

// generateResourceBase produces a spec-correct base object for a resource by its
// `x-resourceId`, reusing the same DataGenerator that serves normal responses.
// This keeps seeded objects faithful to the bundled OpenAPI spec (and therefore
// to Stripe's official schemas) without hand-maintained templates.
func (s *StubServer) generateResourceBase(resourceID string) (map[string]interface{}, error) {
	schema, ok := s.spec.Components.Schemas[resourceID]
	if !ok {
		return nil, fmt.Errorf("schema %q not found in spec", resourceID)
	}

	generator := DataGenerator{s.spec.Components.Schemas, s.fixtures, s.verbose}
	data, err := generator.Generate(&GenerateParams{
		RequestMethod: http.MethodGet,
		RequestPath:   "/v1/" + resourceID + "s",
		Schema:        schema,
	})
	if err != nil {
		return nil, err
	}

	obj, ok := data.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("generated base for %q was not a JSON object", resourceID)
	}
	return obj, nil
}

// decodeJSONObject reads a JSON object from a request body. An empty body is
// treated as an empty object (no overrides), not an error.
func decodeJSONObject(body io.Reader) (map[string]interface{}, error) {
	result := map[string]interface{}{}
	if body == nil {
		return result, nil
	}
	err := json.NewDecoder(body).Decode(&result)
	if err == io.EOF {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// routeResourceID resolves the `x-resourceId` of a route's 200 JSON response
// schema, following a top-level $ref into the component schemas. Returns "" when
// the route has no identifiable resource (e.g. list or binary responses).
func (s *StubServer) routeResourceID(route *stubServerRoute) string {
	response, ok := route.operation.Responses["200"]
	if !ok {
		return ""
	}
	content, ok := response.Content["application/json"]
	if !ok || content.Schema == nil {
		return ""
	}

	schema := content.Schema
	if schema.Ref != "" {
		name := definitionFromJSONPointer(schema.Ref)
		if resolved, ok := s.spec.Components.Schemas[name]; ok {
			schema = resolved
		}
	}
	return schema.XResourceID
}
