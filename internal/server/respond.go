package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/shabilullah/wakil-mcp/internal/config"
)

// maxJSONBodyBytes bounds an admin API request body. Configs are small, so a
// megabyte is already far more than anything legitimate.
const maxJSONBodyBytes = 1 << 20

// writeJSON renders a value as an indented JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		// The header is already sent, so the only option is to stop.
		slog.Default().Debug("writing JSON response failed", "error", err)
	}
}

// apiError is the error body the UI renders.
type apiError struct {
	Error  string   `json:"error"`
	Detail []string `json:"detail,omitempty"`
}

// writeError renders a failure, unwrapping a config validation error into one
// entry per problem so the UI can list them.
func writeError(w http.ResponseWriter, status int, err error) {
	body := apiError{Error: err.Error()}

	var ve *config.ValidationError
	if errors.As(err, &ve) {
		body.Error = "the configuration is not valid"
		body.Detail = ve.Problems
	}
	writeJSON(w, status, body)
}

// readJSON decodes a request body into dst, rejecting unknown fields and
// oversized bodies.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return fmt.Errorf("request body exceeds %d bytes", maxJSONBodyBytes)
		case errors.Is(err, io.EOF):
			return errors.New("request body is empty")
		default:
			return fmt.Errorf("invalid JSON: %w", err)
		}
	}
	return nil
}

// queryInt reads an integer query parameter.
func queryInt(r *http.Request, name string, def int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// methodNotAllowed writes a 405 with an Allow header.
func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
