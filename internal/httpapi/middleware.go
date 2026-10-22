package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

type transportError struct {
	status        int
	code, message string
}

func (e *transportError) Error() string { return e.code }

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func classifyError(err error) *transportError {
	var transport *transportError
	if errors.As(err, &transport) {
		return transport
	}
	switch {
	case errors.Is(err, application.ErrUnauthenticated):
		return &transportError{401, "UNAUTHENTICATED", "A valid Bearer access token is required."}
	case errors.Is(err, application.ErrForbidden):
		return &transportError{403, "FORBIDDEN", "The identity is not allowed to perform this action."}
	case errors.Is(err, application.ErrNotFound):
		return &transportError{404, "NOT_FOUND", "The requested resource was not found."}
	case errors.Is(err, application.ErrConflict):
		return &transportError{409, "CONFLICT", "Resource or idempotency conflict."}
	}
	for _, invalid := range []error{application.ErrInvalidInput, domain.ErrInvalidMoney, domain.ErrInvalidAmount,
		domain.ErrInvalidCurrency, domain.ErrCurrencyMismatch, domain.ErrMoneyOverflow, domain.ErrInvalidTransaction,
		domain.ErrInvalidTransactionAmount, domain.ErrInvalidReference, domain.ErrNegativeBalance} {
		if errors.Is(err, invalid) {
			return &transportError{400, "INVALID_INPUT", "Check required fields, transaction kind, reference and money format."}
		}
	}
	return &transportError{503, "TEMPORARILY_UNAVAILABLE", "Processing could not be confirmed. Retry with the same idempotency key and payload."}
}
func writeError(w http.ResponseWriter, err error) {
	e := classifyError(err)
	if e.status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wallet-api"`)
	}
	if e.status == 503 {
		w.Header().Set("Retry-After", "1")
	}
	var response errorResponse
	response.Error.Code, response.Error.Message = e.code, e.message
	writeJSON(w, e.status, response)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"code":"INTERNAL_ERROR","message":"Response could not be encoded."}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	if classifyError(err).status >= 500 {
		// Não registrar o erro bruto: drivers podem incluir parâmetros e credenciais.
		a.config.Logger.ErrorContext(r.Context(), "HTTP operation unavailable", "correlationId", metadataOf(r).CorrelationID, "route", r.Pattern)
	}
	writeError(w, err)
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &transportError{415, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json."}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &transportError{413, "PAYLOAD_TOO_LARGE", "The request exceeds 1 MiB."}
		}
		return application.ErrInvalidInput
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' || !utf8.Valid(body) {
		return application.ErrInvalidInput
	}
	// Rejeitar chaves duplicadas evita interpretações diferentes do mesmo payload.
	scan := json.NewDecoder(bytes.NewReader(body))
	scan.UseNumber()
	if err := scanJSON(scan, 0); err != nil {
		return application.ErrInvalidInput
	}
	if _, err := scan.Token(); err != io.EOF {
		return application.ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return application.ErrInvalidInput
	}
	return nil
}
func scanJSON(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return application.ErrInvalidInput
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || keys[key] {
				return application.ErrInvalidInput
			}
			keys[key] = true
			if err := scanJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return application.ErrInvalidInput
	}
	_, err = decoder.Token()
	return err
}
func singleHeader(r *http.Request, name string, required bool) (string, error) {
	values := r.Header.Values(name)
	if len(values) == 0 && !required {
		return "", nil
	}
	if len(values) != 1 {
		return "", application.ErrInvalidInput
	}
	value := values[0]
	if value == "" || len(value) > 512 || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return "", application.ErrInvalidInput
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return "", application.ErrInvalidInput
		}
	}
	return value, nil
}
func parseQuery(r *http.Request) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, application.ErrInvalidInput
	}
	return values, nil
}

type metadataKey struct{}

func metadataOf(r *http.Request) application.RequestMetadata {
	value, _ := r.Context().Value(metadataKey{}).(application.RequestMetadata)
	return value
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *responseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(body)
}
func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (a *api) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		correlation, err := singleHeader(r, "X-Correlation-ID", false)
		if err != nil {
			writeError(w, err)
			return
		}
		if correlation == "" {
			correlation = rand.Text()
		}
		causation, err := singleHeader(r, "X-Causation-ID", false)
		if err != nil {
			writeError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		ctx = context.WithValue(ctx, metadataKey{}, application.RequestMetadata{CorrelationID: correlation, CausationID: causation})
		r = r.WithContext(ctx)
		w.Header().Set("X-Correlation-ID", correlation)
		recorder := &responseRecorder{ResponseWriter: w}
		defer func() {
			if recover() != nil {
				a.config.Logger.ErrorContext(ctx, "HTTP handler panic", "correlationId", correlation)
				if recorder.status == 0 {
					writeJSON(recorder, 500, map[string]any{"error": map[string]string{"code": "INTERNAL_ERROR", "message": "An internal error occurred."}})
				}
			}
			if a.config.ObserveHTTP != nil && r.URL.Path != "/metrics" {
				a.config.ObserveHTTP(r.Method, r.Pattern, recorder.status, time.Since(started))
			}
			a.config.Logger.InfoContext(ctx, "HTTP request", "correlationId", correlation, "method", r.Method, "route", r.Pattern, "status", recorder.status, "durationMs", time.Since(started).Milliseconds())
		}()
		next.ServeHTTP(recorder, r)
	})
}
