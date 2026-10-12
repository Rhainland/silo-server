package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// maxAuthRequestBodyBytes bounds JSON bodies on the public v1 auth and
// onboarding endpoints (login, setup, signup, refresh, device start/poll,
// invitation accept). These run before any credential check, so without a
// cap an anonymous caller could make the server buffer an arbitrarily large
// JSON string. The value matches the v2 default structured-body cap
// (apiv2.MaxJSONBodyBytes) so both surfaces accept the same requests.
const maxAuthRequestBodyBytes = 1 << 20

// decodeAuthRequestBody decodes a size-capped JSON request body into dst. It
// writes 413 for an oversized body and 400 for malformed JSON, and returns
// false in both cases. allowEmpty treats an empty body as a zero-value
// request for endpoints whose fields are all optional.
func decodeAuthRequestBody(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthRequestBodyBytes)
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil || (allowEmpty && errors.Is(err, io.EOF)) {
		return true
	}
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "Request body is too large")
		return false
	}
	writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
	return false
}
