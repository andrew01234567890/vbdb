package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrew01234567890/vbdb/internal/storage"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
)

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	var next byte
	store, err := storage.Open(filepath.Join(t.TempDir(), "data"), storage.Options{UUIDGenerator: func() (uuidv7.UUID, error) {
		next++
		return uuidv7.Generator{Now: func() time.Time { return time.UnixMilli(100) }, Rand: bytes.NewReader(bytes.Repeat([]byte{next}, 10))}.New()
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(store).Handler()
}

func request(handler http.Handler, method, target string, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestPutGetAndETagConditions(t *testing.T) {
	handler := testHandler(t)
	created := request(handler, http.MethodPut, "/users/alice", `{"name":"A"}`, map[string]string{"Content-Type": "application/json; charset=utf-8"})
	if created.Code != http.StatusCreated || created.Header().Get("ETag") == "" {
		t.Fatalf("create = %d, headers=%v body=%s", created.Code, created.Header(), created.Body.String())
	}
	etag := created.Header().Get("ETag")
	got := request(handler, http.MethodGet, "/users/alice", "", nil)
	if got.Code != http.StatusOK || got.Header().Get("ETag") != etag {
		t.Fatalf("get = %d etag=%q body=%s", got.Code, got.Header().Get("ETag"), got.Body.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &envelope); err != nil || envelope["_key"] != "alice" {
		t.Fatalf("envelope = %s, %v", got.Body, err)
	}
	if version, ok := envelope["_version"].(string); !ok || `"`+version+`"` != etag {
		t.Fatalf("envelope version and ETag differ: version=%v etag=%q", envelope["_version"], etag)
	}
	notModified := request(handler, http.MethodGet, "/users/alice", "", map[string]string{"If-None-Match": etag})
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 || notModified.Header().Get("ETag") != etag {
		t.Fatalf("exact If-None-Match = %d body=%s", notModified.Code, notModified.Body)
	}
	star := request(handler, http.MethodGet, "/users/alice", "", map[string]string{"If-None-Match": "*"})
	if star.Code != http.StatusNotModified {
		t.Fatalf("star If-None-Match = %d", star.Code)
	}
	blind := request(handler, http.MethodPut, "/users/alice", `{"name":"blind"}`, map[string]string{"Content-Type": "application/json"})
	if blind.Code != http.StatusOK {
		t.Fatalf("blind update = %d body=%s", blind.Code, blind.Body)
	}
	blindETag := blind.Header().Get("ETag")
	updated := request(handler, http.MethodPut, "/users/alice", `{"name":"B"}`, map[string]string{"Content-Type": "application/json", "If-Match": blindETag})
	if updated.Code != http.StatusOK || updated.Header().Get("ETag") == blindETag {
		t.Fatalf("conditional update = %d etag=%q", updated.Code, updated.Header().Get("ETag"))
	}
	stale := request(handler, http.MethodPut, "/users/alice", `null`, map[string]string{"Content-Type": "application/json", "If-Match": etag})
	if stale.Code != http.StatusPreconditionFailed || stale.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("stale update = %d content-type=%q body=%s", stale.Code, stale.Header().Get("Content-Type"), stale.Body)
	}
	createOnly := request(handler, http.MethodPut, "/users/create-only", `{"ok":true}`, map[string]string{"Content-Type": "application/json", "If-None-Match": "*"})
	if createOnly.Code != http.StatusCreated {
		t.Fatalf("create-only create = %d body=%s", createOnly.Code, createOnly.Body)
	}
	createOnlyAgain := request(handler, http.MethodPut, "/users/create-only", `{"ok":false}`, map[string]string{"Content-Type": "application/json", "If-None-Match": "*"})
	if createOnlyAgain.Code != http.StatusPreconditionFailed {
		t.Fatalf("create-only existing = %d body=%s", createOnlyAgain.Code, createOnlyAgain.Body)
	}
}

func TestHTTPValidationAndLimits(t *testing.T) {
	handler := testHandler(t)
	for _, test := range []struct {
		name, method, target, body, contentType, ifMatch, ifNone string
		status                                                   int
	}{
		{name: "missing", method: http.MethodGet, target: "/users/missing", status: http.StatusNotFound},
		{name: "missing malformed if-none-match", method: http.MethodGet, target: "/users/missing", ifNone: `"not-a-uuid"`, status: http.StatusBadRequest},
		{name: "bad path", method: http.MethodGet, target: "/Users/a", status: http.StatusBadRequest},
		{name: "extra segment", method: http.MethodGet, target: "/users/a/b", status: http.StatusBadRequest},
		{name: "reserved transactions", method: http.MethodGet, target: "/transactions/a", status: http.StatusNotFound},
		{name: "reserved admin", method: http.MethodGet, target: "/_admin/a", status: http.StatusNotFound},
		{name: "reserved cdc", method: http.MethodGet, target: "/_cdc/a", status: http.StatusNotFound},
		{name: "encoded slash", method: http.MethodGet, target: "/users/a%2Fb", status: http.StatusBadRequest},
		{name: "method", method: http.MethodDelete, target: "/users/a", status: http.StatusMethodNotAllowed},
		{name: "media", method: http.MethodPut, target: "/users/a", body: `null`, contentType: "text/plain", status: http.StatusUnsupportedMediaType},
		{name: "json", method: http.MethodPut, target: "/users/a", body: `{"x":`, contentType: "application/json", status: http.StatusBadRequest},
		{name: "both conditions", method: http.MethodPut, target: "/users/a", body: `null`, contentType: "application/json", ifMatch: `"01020304-0506-7bcb-8bab-abababababab"`, ifNone: "*", status: http.StatusBadRequest},
		{name: "bad if-match", method: http.MethodPut, target: "/users/a", body: `null`, contentType: "application/json", ifMatch: "*", status: http.StatusBadRequest},
		{name: "bad if-none", method: http.MethodPut, target: "/users/a", body: `null`, contentType: "application/json", ifNone: `"bad"`, status: http.StatusBadRequest},
		{name: "transaction", method: http.MethodPut, target: "/users/a", body: `null`, contentType: "application/json", status: http.StatusNotImplemented},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := map[string]string{}
			if test.contentType != "" {
				headers["Content-Type"] = test.contentType
			}
			if test.ifMatch != "" {
				headers["If-Match"] = test.ifMatch
			}
			if test.ifNone != "" {
				headers["If-None-Match"] = test.ifNone
			}
			if test.name == "transaction" {
				headers["X-Transaction-Id"] = ""
			}
			response := request(handler, test.method, test.target, test.body, headers)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d body=%s", response.Code, test.status, response.Body)
			}
			if test.name == "method" && response.Header().Get("Allow") != "GET, PUT" {
				t.Fatalf("Allow = %q, want GET, PUT", response.Header().Get("Allow"))
			}
		})
	}
	oversized := request(handler, http.MethodPut, "/users/large", strings.Repeat("x", MaxBodyBytes+1), map[string]string{"Content-Type": "application/json"})
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized = %d body=%s", oversized.Code, oversized.Body)
	}
	canonicalOversized := `"` + strings.Repeat("<", 200_000) + `"`
	if len(canonicalOversized) > MaxBodyBytes {
		t.Fatal("canonical expansion test input unexpectedly exceeds request limit")
	}
	canonicalResponse := request(handler, http.MethodPut, "/users/canonical-large", canonicalOversized, map[string]string{"Content-Type": "application/json"})
	if canonicalResponse.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("canonical oversized = %d body=%s", canonicalResponse.Code, canonicalResponse.Body)
	}
	if missing := request(handler, http.MethodGet, "/users/canonical-large", "", nil); missing.Code != http.StatusNotFound {
		t.Fatalf("canonical oversized row status = %d, want 404", missing.Code)
	}
	unknownLength := httptest.NewRequest(http.MethodPut, "/users/unknown-length", nil)
	unknownLength.Header.Set("Content-Type", "application/json")
	unknownLength.ContentLength = -1
	unknownLength.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", MaxBodyBytes+1)))
	unknownLengthResponse := httptest.NewRecorder()
	handler.ServeHTTP(unknownLengthResponse, unknownLength)
	if unknownLengthResponse.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("unknown-length oversized = %d body=%s", unknownLengthResponse.Code, unknownLengthResponse.Body)
	}
}

func TestDuplicateConditionalAndMediaHeadersAreRejected(t *testing.T) {
	handler := testHandler(t)
	created := request(handler, http.MethodPut, "/users/headers", `true`, map[string]string{"Content-Type": "application/json"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d", created.Code)
	}
	etag := created.Header().Get("ETag")
	duplicateNone := httptest.NewRequest(http.MethodGet, "/users/headers", nil)
	duplicateNone.Header["If-None-Match"] = []string{etag}
	duplicateNone.Header["if-none-match"] = []string{etag}
	duplicateNoneResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateNoneResponse, duplicateNone)
	if duplicateNoneResponse.Code != http.StatusBadRequest {
		t.Fatalf("duplicate If-None-Match = %d body=%s", duplicateNoneResponse.Code, duplicateNoneResponse.Body)
	}
	duplicateContentType := httptest.NewRequest(http.MethodPut, "/users/headers", strings.NewReader(`true`))
	duplicateContentType.Header["Content-Type"] = []string{"application/json"}
	duplicateContentType.Header["content-type"] = []string{"application/json"}
	duplicateContentTypeResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateContentTypeResponse, duplicateContentType)
	if duplicateContentTypeResponse.Code != http.StatusBadRequest || !strings.Contains(duplicateContentTypeResponse.Body.String(), "invalid_content_type") {
		t.Fatalf("duplicate Content-Type = %d body=%s", duplicateContentTypeResponse.Code, duplicateContentTypeResponse.Body)
	}
}

func TestBodyReadErrorWithLegacyLimitTextIsNotMisclassified(t *testing.T) {
	handler := testHandler(t)
	req := httptest.NewRequest(http.MethodPut, "/users/read-error", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Body = errorReadCloser{err: errors.New("http: request body too large")}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("legacy body error = %d body=%s, want 400", response.Code, response.Body)
	}
}

func TestStorageRejectionUsesStableProblemEnvelope(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "data"), storage.Options{UUIDGenerator: func() (uuidv7.UUID, error) {
		return uuidv7.UUID{}, errors.New("unused")
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	response := request(New(store).Handler(), http.MethodGet, "/users/closed", "", nil)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("closed storage response = %d content-type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body)
	}
	var problem map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem["code"] != "storage_closed" || problem["detail"] != "The database is not serving requests." || problem["status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("closed storage problem = %#v", problem)
	}
}

func TestStorageProblemBranches(t *testing.T) {
	for name, test := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"terminal": {err: storage.ErrTerminal, status: http.StatusServiceUnavailable, code: "storage_terminal"},
		"closed":   {err: storage.ErrClosed, status: http.StatusServiceUnavailable, code: "storage_closed"},
		"corrupt":  {err: storage.ErrCorrupt, status: http.StatusInternalServerError, code: "storage_corrupt"},
		"generic":  {err: errors.New("internal detail"), status: http.StatusInternalServerError, code: "storage_error"},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeStorageProblem(response, test.err)
			if response.Code != test.status || response.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("storage problem response = %d content-type=%q", response.Code, response.Header().Get("Content-Type"))
			}
			var problem map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
				t.Fatal(err)
			}
			if problem["code"] != test.code || problem["status"] != float64(test.status) || strings.Contains(response.Body.String(), "internal detail") {
				t.Fatalf("storage problem = %#v body=%s", problem, response.Body)
			}
		})
	}
}

type errorReadCloser struct{ err error }

func (r errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r errorReadCloser) Close() error             { return nil }
