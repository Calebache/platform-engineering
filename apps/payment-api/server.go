package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"
)

const maxAmountCents = 10_000_000

var (
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
	uuidRE     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

type server struct {
	store   Store
	metrics *metrics
	logger  *slog.Logger
}

func newServer(store Store, m *metrics, logger *slog.Logger) *server {
	return &server{store: store, metrics: m, logger: logger}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	// Liveness and readiness deliberately do NOT check the database. If they
	// did, a bad DATABASE_HOST would keep new pods unready and the old ones
	// would keep serving, hiding the failure. The demo needs it to surface
	// as HTTP 500s that monitoring can detect.
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /readyz", ok)
	mux.HandleFunc("POST /v1/payments", s.createPayment)
	mux.HandleFunc("GET /v1/payments/{id}", s.getPayment)
	return s.metrics.instrument(s.logRequests(mux))
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}

type createPaymentRequest struct {
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

func (s *server) createPayment(w http.ResponseWriter, r *http.Request) {
	var req createPaymentRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.AmountCents <= 0 || req.AmountCents > maxAmountCents {
		writeError(w, http.StatusBadRequest, "amount_cents must be between 1 and 10000000")
		return
	}
	if !currencyRE.MatchString(req.Currency) {
		writeError(w, http.StatusBadRequest, "currency must be a 3-letter ISO 4217 code")
		return
	}

	p, err := s.store.CreatePayment(r.Context(), req.AmountCents, req.Currency)
	if err != nil {
		s.databaseError(w, r, "create payment", err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *server) getPayment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Reject malformed IDs before they reach Postgres, which would fail the
	// uuid cast with an error that counts as a 5xx and pollutes the
	// error-rate alert.
	if !uuidRE.MatchString(id) {
		writeError(w, http.StatusNotFound, "payment not found")
		return
	}
	p, err := s.store.GetPayment(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "payment not found")
	case err != nil:
		s.databaseError(w, r, "get payment", err)
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

// databaseError logs the real cause (what an investigating agent needs) and
// returns a generic 500 (what a client should see).
func (s *server) databaseError(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.metrics.dbErrors.Inc()
	s.logger.Error("database operation failed",
		"op", op, "request_id", requestID(r), "error", err.Error())
	writeError(w, http.StatusInternalServerError, "internal error")
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Request-Id") == "" {
			r.Header.Set("X-Request-Id", newRequestID())
		}
		w.Header().Set("X-Request-Id", requestID(r))
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		s.logger.Info("request",
			"request_id", requestID(r), "method", r.Method, "route", r.Pattern,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

func requestID(r *http.Request) string { return r.Header.Get("X-Request-Id") }

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
