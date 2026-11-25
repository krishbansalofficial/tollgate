package auth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the Postgres-backed key store.
type PGStore struct{ pool *pgxpool.Pool }

func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// LookupKey matches on the unique index over key_hash. The secret comparison
// happens inside the index lookup, never as a byte-by-byte compare in Go, so
// there is no timing side channel on the raw key.
func (s *PGStore) LookupKey(ctx context.Context, hash []byte) (Key, error) {
	var k Key
	err := s.pool.QueryRow(ctx, `
		SELECT k.id::text, p.id::text, p.name,
		       COALESCE(k.rpm_limit, p.rpm_limit), COALESCE(k.tpm_limit, p.tpm_limit),
		       k.revoked_at IS NOT NULL
		FROM api_keys k JOIN projects p ON p.id = k.project_id
		WHERE k.key_hash = $1`, hash,
	).Scan(&k.ID, &k.ProjectID, &k.ProjectName, &k.RPMLimit, &k.TPMLimit, &k.Revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	return k, err
}

// CreateKey creates the project if needed and inserts a new key. rpm/tpm of 0
// mean "inherit the project's limits". Returns the raw key, shown once.
func (s *PGStore) CreateKey(ctx context.Context, project, name string, rpm, tpm int) (raw string, err error) {
	raw, hash, display, err := Generate()
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var projectID string
	// DO UPDATE (a no-op write) instead of DO NOTHING so RETURNING yields the
	// id for an existing project too.
	err = tx.QueryRow(ctx, `
		INSERT INTO projects (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id::text`, project).Scan(&projectID)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO api_keys (project_id, key_hash, key_prefix, name, rpm_limit, tpm_limit)
		VALUES ($1, $2, $3, $4, NULLIF($5, 0), NULLIF($6, 0))`,
		projectID, hash[:], display, name, rpm, tpm)
	if err != nil {
		return "", err
	}
	return raw, tx.Commit(ctx)
}

// RevokeKey marks every key with the given display prefix revoked. Running
// instances stop accepting it within one cache TTL.
func (s *PGStore) RevokeKey(ctx context.Context, display string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE key_prefix = $1 AND revoked_at IS NULL`, display)
	return tag.RowsAffected(), err
}
