CREATE TABLE gateways (
    consumer    TEXT PRIMARY KEY CHECK (length(consumer) BETWEEN 1 AND 40 AND consumer ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    token_id    TEXT NOT NULL UNIQUE REFERENCES api_tokens (id),
    identities  TEXT[] NOT NULL CHECK (identities <@ ARRAY['openlinker', 'github']::TEXT[]),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE gateway_keys (
    consumer    TEXT NOT NULL REFERENCES gateways (consumer),
    kid         TEXT NOT NULL CHECK (length(kid) BETWEEN 1 AND 64 AND kid ~ '^[A-Za-z0-9._-]+$'),
    public_key  BYTEA NOT NULL CHECK (length(public_key) = 32),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ,
    PRIMARY KEY (consumer, kid)
);

CREATE TABLE gateway_assertion_jtis (
    consumer    TEXT NOT NULL,
    jti         TEXT NOT NULL CHECK (length(jti) BETWEEN 16 AND 128),
    expires_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (consumer, jti)
);
CREATE INDEX gateway_assertion_jtis_expires_idx ON gateway_assertion_jtis (expires_at);
