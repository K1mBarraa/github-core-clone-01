package main

// Response helpers: every reply carries a non-empty X-GitHub-Request-Id,
// JSON replies are `application/json; charset=utf-8`, and error bodies are
// JSON objects with a string `message`.

import (
	"net/http"
	"strconv"
)

// JSONType is the content type of every JSON reply.
const JSONType = "application/json; charset=utf-8"

// applyHeaders sets the headers for one reply: the defaults first, then
// the caller's extras (which may override, matching the TS spread order).
func applyHeaders(w http.ResponseWriter, contentType string, contentLength int, headers map[string]string) {
	h := w.Header()
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	if contentLength >= 0 {
		h.Set("Content-Length", strconv.Itoa(contentLength))
	}
	h.Set("X-GitHub-Request-Id", NextRequestID())
	for k, v := range headers {
		h.Set(k, v)
	}
}

// SendJSON sends a JSON body with a status and extra headers. The body is
// the JSON text plus a trailing newline, exactly like the TS version.
func SendJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	text := JSONText(body) + "\n"
	applyHeaders(w, JSONType, len(text), headers)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(text))
}

// SendJSONText sends an already-serialized JSON body (the hot read path's
// cache), plus a trailing newline.
func SendJSONText(w http.ResponseWriter, status int, text string, headers map[string]string) {
	body := text + "\n"
	applyHeaders(w, JSONType, len(body), headers)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// SendNoContent sends a 204 with no body (a deleted repository).
func SendNoContent(w http.ResponseWriter) {
	applyHeaders(w, "", -1, nil)
	w.WriteHeader(http.StatusNoContent)
}

// SendBytes sends raw bytes with a content type and extra headers.
func SendBytes(w http.ResponseWriter, status int, data []byte, contentType string, headers map[string]string) {
	applyHeaders(w, contentType, len(data), headers)
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// errorBody is the single-key JSON error shape {"message": "..."}.
// A struct (not a one-entry map) keeps the key order fixed and avoids a
// map allocation on every error reply.
type errorBody struct {
	Message string `json:"message"`
}

// SendError sends a JSON error body: {"message": "..."}.
func SendError(w http.ResponseWriter, status int, message string) {
	SendJSON(w, status, errorBody{Message: message}, nil)
}

// SendNotFound sends a 404 JSON error for an unknown route or a missing object.
func SendNotFound(w http.ResponseWriter, what string) {
	SendError(w, http.StatusNotFound, what)
}

// SendUnauthorized sends a 401 JSON error for a missing or wrong bearer token.
func SendUnauthorized(w http.ResponseWriter) {
	SendError(w, http.StatusUnauthorized, "Bad credentials")
}
