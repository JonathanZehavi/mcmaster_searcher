package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgBackend keeps the store's JSON document in one Postgres row, so the data
// survives restarts on hosts without a disk (Render's free plan). Set
// DATABASE_URL to a connection string (e.g. a free Neon database).
type pgBackend struct{ db *sql.DB }

func OpenPostgresStore(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(4 * time.Minute) // free databases drop idle connections
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A sleeping free database can take a few seconds to wake up.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS app_state (
		id int PRIMARY KEY,
		data jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		db.Close()
		return nil, err
	}
	return openStore(pgBackend{db})
}

func (p pgBackend) load() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var b []byte
	err := p.db.QueryRowContext(ctx, `SELECT data FROM app_state WHERE id = 1`).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

func (p pgBackend) write(b []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := p.db.ExecContext(ctx, `INSERT INTO app_state (id, data, updated_at) VALUES (1, $1, now())
		ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data, updated_at = now()`, string(b))
	return err
}
