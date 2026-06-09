package server

import (
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
)

//
// The stateful store backs stripe-mock's test-control extensions (the
// `/v1/_mock/*` endpoints). Upstream stripe-mock is intentionally stateless; this
// store sits beside the generic, spec-driven response generator and is only
// consulted for resources a test has explicitly seeded.
//
// State is partitioned into per-session stores so that parallel test workers
// don't collide. A "session" is identified by the API key in the request's
// Authorization header (tests use a distinct key per worker) or an explicit
// `X-Stripe-Mock-Session` header. See sessionID.
//

// mockSessionHeader lets a client pick a session explicitly, overriding the
// API-key-derived default. Useful when the same key is shared across workers.
const mockSessionHeader = "X-Stripe-Mock-Session"

// defaultSessionID is used when a request carries no usable session identifier.
const defaultSessionID = "default"

// sessionStore holds the stateful resources and queued events for a single mock
// session.
type sessionStore struct {
	// resources is keyed by resource id (e.g. "payment_intent", "charge") and
	// then by object id, holding the stored JSON object.
	resources map[string]map[string]map[string]interface{}

	// events is a FIFO queue of webhook-event envelopes produced by state
	// transitions, drained on demand by the test.
	events []map[string]interface{}
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		resources: make(map[string]map[string]map[string]interface{}),
	}
}

// statefulStore is a collection of per-session stores, safe for concurrent use.
type statefulStore struct {
	mu       sync.RWMutex
	sessions map[string]*sessionStore
}

func newStatefulStore() *statefulStore {
	return &statefulStore{sessions: make(map[string]*sessionStore)}
}

// ensureSession returns the store for a session, creating it lazily. The caller
// must hold s.mu (write lock).
func (s *statefulStore) ensureSession(id string) *sessionStore {
	sess, ok := s.sessions[id]
	if !ok {
		sess = newSessionStore()
		s.sessions[id] = sess
	}
	return sess
}

// reset clears all stateful resources for the given session. Tests call this
// between examples (e.g. in a `before(:each)` hook) to stay isolated.
func (s *statefulStore) reset(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// putResource stores (or replaces) a resource object in the session.
func (s *statefulStore) putResource(session, resourceID, id string, obj map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.ensureSession(session)
	byID, ok := sess.resources[resourceID]
	if !ok {
		byID = make(map[string]map[string]interface{})
		sess.resources[resourceID] = byID
	}
	byID[id] = obj
}

// getResource returns a stored resource object and whether it existed.
func (s *statefulStore) getResource(session, resourceID, id string) (map[string]interface{}, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[session]
	if !ok {
		return nil, false
	}
	byID, ok := sess.resources[resourceID]
	if !ok {
		return nil, false
	}
	obj, ok := byID[id]
	return obj, ok
}

// enqueueEvent appends a webhook-event envelope to the session's queue.
func (s *statefulStore) enqueueEvent(session string, event map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.ensureSession(session)
	sess.events = append(sess.events, event)
}

// drainEvents returns the session's queued events in FIFO order and clears the
// queue. Returns an empty slice when there's nothing queued.
func (s *statefulStore) drainEvents(session string) []map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[session]
	if !ok || len(sess.events) == 0 {
		return []map[string]interface{}{}
	}
	events := sess.events
	sess.events = nil
	return events
}

//
// Resource-specific convenience wrappers.
//

func (s *statefulStore) putPaymentIntent(session, id string, obj map[string]interface{}) {
	s.putResource(session, paymentIntentResourceID, id, obj)
}

func (s *statefulStore) getPaymentIntent(session, id string) (map[string]interface{}, bool) {
	return s.getResource(session, paymentIntentResourceID, id)
}

func (s *statefulStore) putCharge(session, id string, obj map[string]interface{}) {
	s.putResource(session, chargeResourceID, id, obj)
}

func (s *statefulStore) getCharge(session, id string) (map[string]interface{}, bool) {
	return s.getResource(session, chargeResourceID, id)
}

// sessionID derives a mock-session identifier from a request. An explicit
// `X-Stripe-Mock-Session` header wins; otherwise we use the API key from the
// Authorization header (tests set a distinct key per parallel worker). Falls back
// to defaultSessionID when nothing usable is present.
func sessionID(r *http.Request) string {
	if s := r.Header.Get(mockSessionHeader); s != "" {
		return s
	}
	if key := apiKeyFromAuth(r.Header.Get("Authorization")); key != "" {
		return key
	}
	return defaultSessionID
}

// apiKeyFromAuth extracts the raw API key from an Authorization header value,
// handling both `Bearer <key>` and `Basic <base64(key:)>` forms. Returns "" if
// no key can be extracted. This mirrors the parsing in validateAuth but does not
// validate the key's shape — session bucketing should be permissive.
func apiKeyFromAuth(auth string) string {
	parts := strings.Split(auth, " ")
	if len(parts) != 2 || parts[1] == "" {
		return ""
	}

	switch parts[0] {
	case "Bearer":
		return parts[1]
	case "Basic":
		keyBytes, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
		// Stripe sends the key as the Basic-auth username with an empty
		// password, which decodes to "<key>:". Drop the trailing colon so the
		// Basic and Bearer forms of the same key map to one session.
		return strings.TrimSuffix(string(keyBytes), ":")
	default:
		return ""
	}
}

// mergeMap recursively merges src into dst and returns dst. Nested objects are
// merged key-by-key; any other value (scalars, arrays, null) in src replaces the
// corresponding value in dst wholesale. This powers "base + overrides" seeding:
// callers send only the fields they care about and the spec-generated base
// supplies the rest.
func mergeMap(dst, src map[string]interface{}) map[string]interface{} {
	for k, srcVal := range src {
		if srcMap, ok := srcVal.(map[string]interface{}); ok {
			if dstMap, ok := dst[k].(map[string]interface{}); ok {
				dst[k] = mergeMap(dstMap, srcMap)
				continue
			}
		}
		dst[k] = srcVal
	}
	return dst
}
