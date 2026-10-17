-- payment-api database schema (Postgres 18).
--
-- This file is the single source of truth: the service embeds it and applies
-- it at startup, and the MCP server serves it to the SRE agent as
-- platform://services/payment-api/schema. Statements must stay idempotent.

CREATE TABLE IF NOT EXISTS payments (
	id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
	amount_cents bigint      NOT NULL CHECK (amount_cents > 0),
	currency     char(3)     NOT NULL,                  -- ISO 4217, upper case
	status       text        NOT NULL DEFAULT 'captured',
	created_at   timestamptz NOT NULL DEFAULT now()
);
