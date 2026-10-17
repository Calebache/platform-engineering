package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	payments map[string]Payment
	err      error // returned by every call when set (simulates a dead DB)
}

func (f *fakeStore) CreatePayment(_ context.Context, amount int64, currency string) (Payment, error) {
	if f.err != nil {
		return Payment{}, f.err
	}
	p := Payment{ID: "0b6c6e0e-2f5f-4d39-9f3e-6a1f2d0c9a11", AmountCents: amount, Currency: currency, Status: "captured", CreatedAt: time.Unix(0, 0).UTC()}
	f.payments[p.ID] = p
	return p, nil
}

func (f *fakeStore) GetPayment(_ context.Context, id string) (Payment, error) {
	if f.err != nil {
		return Payment{}, f.err
	}
	p, ok := f.payments[id]
	if !ok {
		return Payment{}, ErrNotFound
	}
	return p, nil
}

func newTestServer(store Store) (http.Handler, *metrics) {
	m := newMetrics()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return newServer(store, m, logger).routes(), m
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateAndGetPayment(t *testing.T) {
	h, _ := newTestServer(&fakeStore{payments: map[string]Payment{}})

	rec := do(t, h, http.MethodPost, "/v1/payments", `{"amount_cents":1999,"currency":"CAD"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var p Payment
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.AmountCents != 1999 || p.Currency != "CAD" {
		t.Fatalf("unexpected payment %+v", p)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("missing X-Request-Id header")
	}

	if rec := do(t, h, http.MethodGet, "/v1/payments/"+p.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("get: got %d, want 200", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/v1/payments/00000000-0000-0000-0000-000000000000", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get missing: got %d, want 404", rec.Code)
	}
}

// Malformed IDs must be a client error, never a 5xx: they would otherwise
// trip the error-rate alert. The store is "down" here to prove the request
// never reaches it.
func TestMalformedIDIsNotFoundWithoutTouchingStore(t *testing.T) {
	h, _ := newTestServer(&fakeStore{err: errors.New("store must not be called")})
	if rec := do(t, h, http.MethodGet, "/v1/payments/not-a-uuid", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestCreatePaymentValidation(t *testing.T) {
	h, _ := newTestServer(&fakeStore{payments: map[string]Payment{}})
	for name, body := range map[string]string{
		"not json":       `{`,
		"zero amount":    `{"amount_cents":0,"currency":"CAD"}`,
		"too large":      `{"amount_cents":10000001,"currency":"CAD"}`,
		"bad currency":   `{"amount_cents":100,"currency":"cad"}`,
		"unknown fields": `{"amount_cents":100,"currency":"CAD","admin":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := do(t, h, http.MethodPost, "/v1/payments", body); rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", rec.Code)
			}
		})
	}
}

// The demo's failure mode: the database is unreachable, so requests fail with
// a generic 500 while health checks stay green.
func TestDatabaseDownReturns500ButStaysHealthy(t *testing.T) {
	h, m := newTestServer(&fakeStore{err: errors.New("dial tcp: lookup wrong-host: no such host")})

	rec := do(t, h, http.MethodPost, "/v1/payments", `{"amount_cents":100,"currency":"CAD"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("create: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "wrong-host") {
		t.Error("internal error details leaked to the client")
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := do(t, h, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Errorf("%s: got %d, want 200", path, rec.Code)
		}
	}

	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`http_requests_total{code="500",method="POST",route="POST /v1/payments"} 1`,
		`payment_database_errors_total 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestDBConfigFromEnv(t *testing.T) {
	t.Setenv("DATABASE_HOST", "postgres")
	t.Setenv("DATABASE_USER", "payments")
	t.Setenv("DATABASE_PASSWORD", "p@ss word")
	cfg, err := dbConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.connString(); !strings.HasPrefix(got, "postgres://payments:p%40ss%20word@postgres:5432/payments?") {
		t.Fatalf("unexpected connection string %q", got)
	}

	t.Setenv("DATABASE_PASSWORD", "")
	if _, err := dbConfigFromEnv(); err == nil {
		t.Fatal("expected error when password is missing")
	}
}
