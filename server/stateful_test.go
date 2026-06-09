package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	assert "github.com/stretchr/testify/require"
)

func TestMergeMap(t *testing.T) {
	base := map[string]interface{}{
		"a":      1,
		"nested": map[string]interface{}{"x": "old", "keep": true},
		"arr":    []interface{}{1, 2},
	}
	overrides := map[string]interface{}{
		"a":      2,
		"nested": map[string]interface{}{"x": "new", "added": 5},
		"arr":    []interface{}{9},
		"b":      "added",
	}

	got := mergeMap(base, overrides)

	assert.Equal(t, 2, got["a"], "scalar replaced")
	assert.Equal(t, "added", got["b"], "new key added")

	nested := got["nested"].(map[string]interface{})
	assert.Equal(t, "new", nested["x"], "nested scalar replaced")
	assert.Equal(t, true, nested["keep"], "untouched nested key preserved")
	assert.Equal(t, 5, nested["added"], "new nested key added")

	assert.Equal(t, []interface{}{9}, got["arr"], "arrays replace wholesale, not merge")
}

func TestApiKeyFromAuth(t *testing.T) {
	assert.Equal(t, "sk_test_123", apiKeyFromAuth("Bearer sk_test_123"))
	assert.Equal(t, "sk_test_123",
		apiKeyFromAuth("Basic "+base64.StdEncoding.EncodeToString([]byte("sk_test_123:"))),
		"Basic form strips the trailing colon so it matches the Bearer form")
	assert.Equal(t, "", apiKeyFromAuth(""))
	assert.Equal(t, "", apiKeyFromAuth("Bearer"))
	assert.Equal(t, "", apiKeyFromAuth("Bearer "))
	assert.Equal(t, "", apiKeyFromAuth("Digest abc"))
}

func TestSessionID(t *testing.T) {
	withAuth := httptest.NewRequest(http.MethodGet, "/", nil)
	withAuth.Header.Set("Authorization", "Bearer sk_test_abc")
	assert.Equal(t, "sk_test_abc", sessionID(withAuth), "derived from API key")

	withHeader := httptest.NewRequest(http.MethodGet, "/", nil)
	withHeader.Header.Set("Authorization", "Bearer sk_test_abc")
	withHeader.Header.Set(mockSessionHeader, "explicit-session")
	assert.Equal(t, "explicit-session", sessionID(withHeader), "explicit header wins over the key")

	bare := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.Equal(t, defaultSessionID, sessionID(bare), "falls back to the default session")
}

func TestStatefulStorePaymentIntent(t *testing.T) {
	store := newStatefulStore()

	_, ok := store.getPaymentIntent("s1", "pi_1")
	assert.False(t, ok, "nothing stored yet")

	store.putPaymentIntent("s1", "pi_1", map[string]interface{}{"id": "pi_1", "amount": 100})
	got, ok := store.getPaymentIntent("s1", "pi_1")
	assert.True(t, ok)
	assert.Equal(t, "pi_1", got["id"])

	_, ok = store.getPaymentIntent("s2", "pi_1")
	assert.False(t, ok, "sessions are isolated")

	store.reset("s1")
	_, ok = store.getPaymentIntent("s1", "pi_1")
	assert.False(t, ok, "reset clears the session")
}
