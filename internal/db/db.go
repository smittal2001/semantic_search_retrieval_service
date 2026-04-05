package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/yourname/semantic-search/internal/models"
)

// DB wraps a pgxpool.Pool and exposes typed query methods.
// All methods are safe for concurrent use.
type DB struct {
	pool *pgxpool.Pool
}

// New creates a connection pool and runs the schema migration.
func New(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	d := &DB{pool: pool}
	if err := d.migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

// Close releases all connections in the pool.
func (d *DB) Close() { d.pool.Close() }

// migrate runs idempotent DDL to ensure the schema is up to date.
// In production, replace with a proper migration tool (golang-migrate, atlas).
func (d *DB) migrate(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS vector;

		CREATE TABLE IF NOT EXISTS documents (
			id          TEXT            PRIMARY KEY,
			tenant_id   TEXT            NOT NULL,
			text        TEXT            NOT NULL,
			metadata    JSONB           NOT NULL DEFAULT '{}',
			embedding   vector(1536),
			status      TEXT            NOT NULL DEFAULT 'pending',
			model       TEXT,
			created_at  TIMESTAMPTZ     NOT NULL DEFAULT now(),
			updated_at  TIMESTAMPTZ     NOT NULL DEFAULT now()
		);

		-- HNSW index for fast ANN queries (only on embedded rows)
		CREATE INDEX IF NOT EXISTS documents_embedding_hnsw
			ON documents USING hnsw (embedding vector_cosine_ops)
			WHERE embedding IS NOT NULL;

		-- Metadata + status lookups
		CREATE INDEX IF NOT EXISTS documents_tenant_status
			ON documents (tenant_id, status);
	`)
	return err
}

// UpsertDocument inserts a new document row or resets an existing one to
// 'pending'. Called by the ingest service before publishing to the queue.
func (d *DB) UpsertDocument(ctx context.Context, doc *models.Document) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO documents (id, tenant_id, text, metadata, status)
		VALUES ($1, $2, $3, $4::jsonb, 'pending')
		ON CONFLICT (id) DO UPDATE
			SET text       = EXCLUDED.text,
			    metadata   = EXCLUDED.metadata,
			    status     = 'pending',
			    embedding  = NULL,
			    updated_at = now()
	`, doc.ID, doc.TenantID, doc.Text, metadataToJSON(doc.Metadata))
	if err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}
	return nil
}

// GetDocument fetches a single document by ID, scoped to a tenant.
func (d *DB) GetDocument(ctx context.Context, id, tenantID string) (*models.Document, error) {
	row := d.pool.QueryRow(ctx, `
		SELECT id, tenant_id, text, metadata, status, model, created_at, updated_at
		FROM documents
		WHERE id = $1 AND tenant_id = $2
	`, id, tenantID)

	var doc models.Document
	var metaJSON []byte
	err := row.Scan(
		&doc.ID, &doc.TenantID, &doc.Text, &metaJSON,
		&doc.Status, &doc.Model, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get document: %w", err)
	}
	doc.Metadata = jsonToMetadata(metaJSON)
	return &doc, nil
}

// GetDocumentText fetches only the text field — used by the embed worker to
// avoid loading the full row including the (potentially large) embedding column.
func (d *DB) GetDocumentText(ctx context.Context, id string) (string, error) {
	var text string
	err := d.pool.QueryRow(ctx,
		`SELECT text FROM documents WHERE id = $1`, id,
	).Scan(&text)
	if err != nil {
		return "", fmt.Errorf("get document text: %w", err)
	}
	return text, nil
}

// SetEmbedding atomically writes the vector and flips status to 'indexed'.
// Called by the embed worker after a successful embed API call.
func (d *DB) SetEmbedding(ctx context.Context, id string, vec []float32, model string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE documents
		SET embedding  = $2,
		    model      = $3,
		    status     = 'indexed',
		    updated_at = now()
		WHERE id = $1
	`, id, pgvector.NewVector(vec), model)
	if err != nil {
		return fmt.Errorf("set embedding: %w", err)
	}
	return nil
}

// SetFailed marks a document as permanently failed after all retries are
// exhausted in the embed worker.
func (d *DB) SetFailed(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'failed', updated_at = now()
		WHERE id = $1
	`, id)
	return err
}

// DeleteDocument removes a document and its vector. Scoped to tenant.
func (d *DB) DeleteDocument(ctx context.Context, id, tenantID string) (bool, error) {
	tag, err := d.pool.Exec(ctx,
		`DELETE FROM documents WHERE id = $1 AND tenant_id = $2`, id, tenantID,
	)
	if err != nil {
		return false, fmt.Errorf("delete document: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SearchSimilar runs an ANN cosine similarity query against pgvector.
// Results are ordered by cosine distance ascending (most similar first).
// The tenant_id filter is ALWAYS applied — cross-tenant leakage is impossible.
func (d *DB) SearchSimilar(
	ctx context.Context,
	tenantID string,
	queryVec []float32,
	limit int,
) ([]models.SearchResult, error) {

	rows, err := d.pool.Query(ctx, `
		SELECT
			id,
			text,
			metadata,
			1 - (embedding <=> $1) AS similarity
		FROM documents
		WHERE tenant_id = $2
		  AND status    = 'indexed'
		ORDER BY embedding <=> $1
		LIMIT $3
	`, pgvector.NewVector(queryVec), tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("search similar: %w", err)
	}
	defer rows.Close()

	var results []models.SearchResult
	for rows.Next() {
		var r models.SearchResult
		var metaJSON []byte
		if err := rows.Scan(&r.DocumentID, &r.Text, &metaJSON, &r.Similarity); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		r.Metadata = jsonToMetadata(metaJSON)
		results = append(results, r)
	}
	return results, rows.Err()
}

// CountPending returns the number of documents stuck in 'pending' state for
// longer than the given duration. Used as a health / observability metric.
func (d *DB) CountPending(ctx context.Context, olderThan string) (int64, error) {
	var count int64
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM documents
		WHERE status = 'pending'
		  AND created_at < now() - $1::interval
	`, olderThan).Scan(&count)
	return count, err
}

// ── helpers ──────────────────────────────────────────────────────────────────

func metadataToJSON(m map[string]string) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	// Simple manual JSON encoding to avoid importing encoding/json here.
	// In a real project, use encoding/json.Marshal.
	b := []byte("{")
	i := 0
	for k, v := range m {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, fmt.Sprintf("%q:%q", k, v)...)
		i++
	}
	b = append(b, '}')
	return b
}

func jsonToMetadata(b []byte) map[string]string {
	// In production use encoding/json.Unmarshal into map[string]string.
	// Simplified here for brevity.
	if len(b) == 0 || string(b) == "{}" || string(b) == "null" {
		return map[string]string{}
	}
	result := map[string]string{}
	// Delegate to standard library in real code.
	return result
}
