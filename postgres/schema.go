package postgres

// SchemaUpSQL is the forward DDL from the v0.1 schema migration, minus Goose's
// `-- +goose Up/Down` comment markers. Useful for consumers using a different
// migration tool (Flyway, Alembic, hand-rolled).
const SchemaUpSQL = `
CREATE TABLE photopicker_oauth_tokens (
    user_id       VARCHAR(255) PRIMARY KEY,
    refresh_token BYTEA NOT NULL,
    access_token  BYTEA,
    expires_at    TIMESTAMPTZ,
    scopes        TEXT[] NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE photopicker_imports (
    id              VARCHAR(50) PRIMARY KEY,
    user_id         VARCHAR(255) NOT NULL,
    session_id      TEXT NOT NULL,
    status          VARCHAR(20) NOT NULL DEFAULT 'pending',
    total_items     INT NOT NULL DEFAULT 0,
    completed_items INT NOT NULL DEFAULT 0,
    failed_items    INT NOT NULL DEFAULT 0,
    image_urls      JSONB NOT NULL DEFAULT '[]',
    error           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_photopicker_imports_user_status ON photopicker_imports(user_id, status);
CREATE INDEX idx_photopicker_imports_pending ON photopicker_imports(status, created_at) WHERE status = 'pending';
`

// SchemaDownSQL is the reverse DDL, for completeness.
const SchemaDownSQL = `
DROP TABLE IF EXISTS photopicker_imports;
DROP TABLE IF EXISTS photopicker_oauth_tokens;
`
