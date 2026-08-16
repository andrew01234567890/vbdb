// Package httpapi implements the intentionally small Milestone 2 HTTP
// contract. It is a single-process development API, not a distributed gateway.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/andrew01234567890/vbdb/internal/coordinates"
	"github.com/andrew01234567890/vbdb/internal/storage"
	"github.com/andrew01234567890/vbdb/pkg/jsondoc"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
)

const (
	MaxBodyBytes = storage.MaxValueBytes
)

// Server is an HTTP handler backed by one storage engine.
type Server struct {
	store *storage.Engine
}

// New constructs a directly testable HTTP handler.
func New(store *storage.Engine) *Server { return &Server{store: store} }

// Handler returns the request handler. No socket is opened by this package.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.store == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "The database is not serving requests.")
		return
	}
	if _, present := valuesForHeader(r.Header, "X-Transaction-Id"); present {
		writeProblem(w, http.StatusNotImplemented, "transactions_not_implemented", "Transactions are reserved for a later milestone.")
		return
	}
	table, key, err := parsePath(r.URL)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_path", "The request path is not a valid table and key.")
		return
	}
	if coordinates.IsReservedTable(table) {
		writeProblem(w, http.StatusNotFound, "reserved_table", "This table name is reserved for a later milestone.")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, r, table, key)
	case http.MethodPut:
		s.handlePut(w, r, table, key)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only GET and PUT are supported.")
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, table, key string) {
	if values, present := valuesForHeader(r.Header, "If-None-Match"); present {
		if len(values) != 1 || !validNone(values[0]) {
			writeProblem(w, http.StatusBadRequest, "invalid_if_none_match", "If-None-Match must be * or one strong quoted UUIDv7.")
			return
		}
	}
	row, err := s.store.Get(table, key)
	if errors.Is(err, storage.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "The requested row does not exist.")
		return
	}
	if err != nil {
		writeStorageProblem(w, err)
		return
	}
	etag := quoteETag(row.Version)
	if values, present := valuesForHeader(r.Header, "If-None-Match"); present {
		matched, _ := matchNone(values[0], etag)
		if matched {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
	writeEnvelope(w, row)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, table, key string) {
	condition, err := parsePutCondition(r.Header)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_condition", "The write condition is malformed or combines unsupported forms.")
		return
	}
	contentTypes, contentTypePresent := valuesForHeader(r.Header, "Content-Type")
	if !contentTypePresent || len(contentTypes) == 0 {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "PUT requires Content-Type: application/json.")
		return
	}
	if len(contentTypes) != 1 {
		writeProblem(w, http.StatusBadRequest, "invalid_content_type", "Content-Type must contain exactly one media type.")
		return
	}
	contentType := contentTypes[0]
	if strings.TrimSpace(contentType) == "" {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "PUT requires Content-Type: application/json.")
		return
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_content_type", "The Content-Type header is malformed.")
		return
	}
	if mediaType != "application/json" {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "PUT requires Content-Type: application/json.")
		return
	}
	if r.ContentLength > MaxBodyBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "body_too_large", "The JSON document exceeds the 1 MiB limit.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, http.StatusRequestEntityTooLarge, "body_too_large", "The JSON document exceeds the 1 MiB limit.")
			return
		}
		writeProblem(w, http.StatusBadRequest, "invalid_body", "The request body could not be read.")
		return
	}
	// Canonicalize before the durable commit so request-size accounting applies
	// to the exact bytes that storage will validate and persist.
	canonical, err := canonicalJSON(body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", "The request body is not a valid JSON document.")
		return
	}
	if len(canonical) > MaxBodyBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "body_too_large", "The canonical JSON document exceeds the 1 MiB limit.")
		return
	}
	result, err := s.store.Put(table, key, canonical, condition)
	if errors.Is(err, storage.ErrPrecondition) {
		writeProblem(w, http.StatusPreconditionFailed, "precondition_failed", "The row changed or already exists.")
		return
	}
	if errors.Is(err, storage.ErrInvalidJSON) || errors.Is(err, storage.ErrNonCanonicalJSON) {
		writeProblem(w, http.StatusBadRequest, "invalid_json", "The request body is not canonical JSON.")
		return
	}
	if err != nil {
		writeStorageProblem(w, err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", quoteETag(result.Row.Version))
	w.WriteHeader(status)
	writeEnvelope(w, result.Row)
}

func parsePath(parsed *url.URL) (string, string, error) {
	escaped := parsed.EscapedPath()
	if escaped == "" || !strings.HasPrefix(escaped, "/") || strings.HasSuffix(escaped, "/") {
		return "", "", errors.New("invalid path shape")
	}
	parts := strings.Split(escaped[1:], "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("path must contain exactly two segments")
	}
	table, err := url.PathUnescape(parts[0])
	if err != nil || (!coordinates.ValidTable(table) && !coordinates.IsReservedTable(table)) {
		return "", "", errors.New("invalid table")
	}
	key, err := url.PathUnescape(parts[1])
	if err != nil || !coordinates.ValidKey(key) {
		return "", "", errors.New("invalid key")
	}
	return table, key, nil
}

func parsePutCondition(header http.Header) (storage.Condition, error) {
	matchValues, matchPresent := valuesForHeader(header, "If-Match")
	noneValues, nonePresent := valuesForHeader(header, "If-None-Match")
	if matchPresent && (len(matchValues) != 1 || strings.TrimSpace(matchValues[0]) == "") {
		return storage.Condition{}, errors.New("invalid If-Match")
	}
	if nonePresent && (len(noneValues) != 1 || strings.TrimSpace(noneValues[0]) == "") {
		return storage.Condition{}, errors.New("invalid If-None-Match")
	}
	if matchPresent && nonePresent {
		return storage.Condition{}, errors.New("conditions cannot be combined")
	}
	if nonePresent {
		if strings.TrimSpace(noneValues[0]) != "*" {
			return storage.Condition{}, errors.New("If-None-Match must be *")
		}
		return storage.Condition{CreateOnly: true}, nil
	}
	if !matchPresent {
		return storage.Condition{}, nil
	}
	value := strings.TrimSpace(matchValues[0])
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' || strings.Contains(value[1:len(value)-1], "\"") {
		return storage.Condition{}, errors.New("If-Match must be one strong quoted UUIDv7")
	}
	version, err := uuidv7.Parse(value[1 : len(value)-1])
	if err != nil {
		return storage.Condition{}, errors.New("If-Match must be one strong quoted UUIDv7")
	}
	return storage.Condition{IfMatch: &version}, nil
}

func valuesForHeader(header http.Header, name string) ([]string, bool) {
	var values []string
	present := false
	for key, headerValues := range header {
		if strings.EqualFold(key, name) {
			present = true
			values = append(values, headerValues...)
		}
	}
	return values, present
}

func matchNone(value, current string) (bool, bool) {
	value = strings.TrimSpace(value)
	if !validNone(value) {
		return false, false
	}
	return value == "*" || value == current, true
}

func validNone(value string) bool {
	value = strings.TrimSpace(value)
	if value == "*" {
		return true
	}
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' || strings.Contains(value[1:len(value)-1], "\"") {
		return false
	}
	version, err := uuidv7.Parse(value[1 : len(value)-1])
	return err == nil && version.String() == value[1:len(value)-1]
}

func canonicalJSON(body []byte) ([]byte, error) {
	// Keep all document validation in pkg/jsondoc, including duplicate-key and
	// lossless-number checks.
	document, err := jsondoc.Parse(body)
	if err != nil {
		return nil, err
	}
	return document.Bytes(), nil
}

func quoteETag(version uuidv7.UUID) string { return `"` + version.String() + `"` }

func writeEnvelope(w io.Writer, row storage.Row) {
	// Both fields are strings, so their JSON encoding cannot fail. A write
	// failure after the response headers are emitted cannot change the result.
	key, _ := json.Marshal(row.Key)
	version, _ := json.Marshal(row.Version.String())
	var envelope bytes.Buffer
	envelope.WriteString(`{"_key":`)
	envelope.Write(key)
	envelope.WriteString(`,"_version":`)
	envelope.Write(version)
	envelope.WriteString(`,"value":`)
	envelope.Write(row.Value)
	envelope.WriteByte('}')
	_, _ = w.Write(envelope.Bytes())
}

// WriteProblem emits the stable M2 application/problem+json error envelope.
// It is exported so the gateway's admission guard uses the same contract as
// storage and request-validation failures.
func WriteProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	problem := struct {
		Code   string `json:"code"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}{Code: code, Title: http.StatusText(status), Status: status, Detail: detail}
	_ = json.NewEncoder(w).Encode(problem)
}

func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	WriteProblem(w, status, code, detail)
}

func writeStorageProblem(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrTerminal) {
		writeProblem(w, http.StatusServiceUnavailable, "storage_terminal", "The database encountered a terminal storage failure.")
		return
	}
	if errors.Is(err, storage.ErrClosed) {
		writeProblem(w, http.StatusServiceUnavailable, "storage_closed", "The database is not serving requests.")
		return
	}
	if errors.Is(err, storage.ErrCorrupt) {
		writeProblem(w, http.StatusInternalServerError, "storage_corrupt", "The database contains invalid persisted data.")
		return
	}
	writeProblem(w, http.StatusInternalServerError, "storage_error", "The database could not complete the request.")
}
