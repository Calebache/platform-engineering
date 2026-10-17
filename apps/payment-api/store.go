package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Payment is a captured payment.
type Payment struct {
	ID          string    `json:"id"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// ErrNotFound is returned when a payment ID does not exist.
var ErrNotFound = errors.New("payment not found")

// Store is the persistence the HTTP layer depends on.
type Store interface {
	CreatePayment(ctx context.Context, amountCents int64, currency string) (Payment, error)
	GetPayment(ctx context.Context, id string) (Payment, error)
}

type dbConfig struct {
	Host, Port, Name, User, Password string
}

func dbConfigFromEnv() (dbConfig, error) {
	cfg := dbConfig{
		Host:     os.Getenv("DATABASE_HOST"),
		Port:     envOr("DATABASE_PORT", "5432"),
		Name:     envOr("DATABASE_NAME", "payments"),
		User:     os.Getenv("DATABASE_USER"),
		Password: os.Getenv("DATABASE_PASSWORD"),
	}
	if cfg.Host == "" || cfg.User == "" || cfg.Password == "" {
		return cfg, errors.New("DATABASE_HOST, DATABASE_USER and DATABASE_PASSWORD are required")
	}
	if _, err := strconv.Atoi(cfg.Port); err != nil {
		return cfg, fmt.Errorf("DATABASE_PORT %q is not a number", cfg.Port)
	}
	return cfg, nil
}

func (c dbConfig) connString() string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.User, c.Password),
		Host:     net.JoinHostPort(c.Host, c.Port),
		Path:     c.Name,
		RawQuery: "sslmode=disable&connect_timeout=2",
	}
	return u.String()
}

type postgresStore struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	mu          sync.Mutex
	schemaReady bool
}

// newPostgresStore never fails because the database is unreachable: the pool
// connects lazily, so a bad DATABASE_HOST surfaces as per-request errors
// (HTTP 500) rather than a crash-looping pod.
func newPostgresStore(ctx context.Context, cfg dbConfig, logger *slog.Logger) (*postgresStore, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.connString())
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}
	poolCfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	s := &postgresStore{pool: pool, logger: logger}
	if err := s.ensureSchema(ctx); err != nil {
		logger.Warn("database not reachable at startup; will retry per request",
			"database_host", cfg.Host, "error", err)
	}
	return s, nil
}

func (s *postgresStore) Close() { s.pool.Close() }

// schema is applied idempotently on first use. The same file is what the MCP
// server serves to the agent, so the documented and running schema can't drift.
//
//go:embed schema.sql
var schema string

func (s *postgresStore) ensureSchema(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaReady {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return err
	}
	s.schemaReady = true
	return nil
}

func (s *postgresStore) CreatePayment(ctx context.Context, amountCents int64, currency string) (Payment, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return Payment{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var p Payment
	err := s.pool.QueryRow(ctx,
		`INSERT INTO payments (amount_cents, currency) VALUES ($1, $2)
		 RETURNING id::text, amount_cents, currency, status, created_at`,
		amountCents, currency,
	).Scan(&p.ID, &p.AmountCents, &p.Currency, &p.Status, &p.CreatedAt)
	return p, err
}

func (s *postgresStore) GetPayment(ctx context.Context, id string) (Payment, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return Payment{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var p Payment
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, amount_cents, currency, status, created_at FROM payments WHERE id = $1`,
		id,
	).Scan(&p.ID, &p.AmountCents, &p.Currency, &p.Status, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	return p, err
}
