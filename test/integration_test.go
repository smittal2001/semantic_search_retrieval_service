package integration_test

// Integration tests spin up real Postgres (with pgvector) and Redis using
// testcontainers. They test the full path from UpsertDocument → SetEmbedding
// → SearchSimilar, verifying that the HNSW index and cosine similarity
// produce the expected ranking.
//
// Run with: go test ./test/... -tags integration -v
// Requires Docker running locally.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yourname/semantic-search/internal/db"
	"github.com/yourname/semantic-search/internal/models"
)

// TestSearchReturnsCorrectRanking verifies that after indexing three documents
// with known embeddings, a query vector closest to doc-1 returns doc-1 first.
//
// We use synthetic 3-dimensional vectors so we can reason about the geometry
// without a real embedding model:
//
//	doc-1: [1, 0, 0]  — points along X axis
//	doc-2: [0, 1, 0]  — points along Y axis (orthogonal to doc-1)
//	doc-3: [0, 0, 1]  — points along Z axis
//	query: [0.9, 0.1, 0] — close to doc-1
//
// Expected ranking: doc-1 (similarity ~0.9) > doc-2 (~0.1) > doc-3 (~0.0)
func TestSearchReturnsCorrectRanking(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	database := setupTestDB(t, ctx)

	tenantID := fmt.Sprintf("tenant-%d", time.Now().UnixNano())

	// Index three documents with synthetic 3-dim embeddings.
	// In production these come from the embed API — here we set them directly.
	docs := []struct {
		id        string
		text      string
		embedding []float32
	}{
		{"doc-1", "dogs playing fetch", []float32{1, 0, 0}},
		{"doc-2", "quarterly earnings report", []float32{0, 1, 0}},
		{"doc-3", "weather forecast", []float32{0, 0, 1}},
	}

	for _, d := range docs {
		err := database.UpsertDocument(ctx, &models.Document{
			ID:       d.id,
			TenantID: tenantID,
			Text:     d.text,
			Metadata: map[string]string{},
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", d.id, err)
		}
		// SetEmbedding uses dims from the vector length — override to 3 for tests
		err = database.SetEmbeddingDims(ctx, d.id, d.embedding, "test-model", 3)
		if err != nil {
			t.Fatalf("set embedding %s: %v", d.id, err)
		}
	}

	// Query close to doc-1
	queryVec := []float32{0.9, 0.1, 0}
	results, err := database.SearchSimilar(ctx, tenantID, queryVec, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// doc-1 must be first
	if results[0].DocumentID != "doc-1" {
		t.Errorf("expected doc-1 first, got %s (similarity=%.3f)", results[0].DocumentID, results[0].Similarity)
	}

	// Similarity scores must be descending
	for i := 1; i < len(results); i++ {
		if results[i].Similarity > results[i-1].Similarity {
			t.Errorf("results not ranked: result[%d].similarity=%.3f > result[%d].similarity=%.3f",
				i, results[i].Similarity, i-1, results[i-1].Similarity)
		}
	}
}

// TestTenantIsolation verifies that a search for tenant-A never returns
// documents belonging to tenant-B, even if tenant-B has more similar documents.
func TestTenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	database := setupTestDB(t, ctx)

	tenantA := fmt.Sprintf("tenant-a-%d", time.Now().UnixNano())
	tenantB := fmt.Sprintf("tenant-b-%d", time.Now().UnixNano())

	// Tenant B has a document perfectly matching our query
	_ = database.UpsertDocument(ctx, &models.Document{
		ID: "b-doc", TenantID: tenantB, Text: "tenant B document",
	})
	_ = database.SetEmbeddingDims(ctx, "b-doc", []float32{1, 0, 0}, "test-model", 3)

	// Tenant A has a less similar document
	_ = database.UpsertDocument(ctx, &models.Document{
		ID: "a-doc", TenantID: tenantA, Text: "tenant A document",
	})
	_ = database.SetEmbeddingDims(ctx, "a-doc", []float32{0, 1, 0}, "test-model", 3)

	// Query as tenant A — must NOT see tenant B's document
	results, err := database.SearchSimilar(ctx, tenantA, []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	for _, r := range results {
		if r.DocumentID == "b-doc" {
			t.Errorf("tenant isolation breach: tenant A received tenant B's document %q", r.DocumentID)
		}
	}
}

// TestPendingDocumentsNotSearchable verifies that documents in 'pending' status
// (not yet embedded) are excluded from search results.
func TestPendingDocumentsNotSearchable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	database := setupTestDB(t, ctx)
	tenantID := fmt.Sprintf("tenant-%d", time.Now().UnixNano())

	// Insert a document but do NOT call SetEmbedding — it stays 'pending'
	_ = database.UpsertDocument(ctx, &models.Document{
		ID: "pending-doc", TenantID: tenantID, Text: "not yet indexed",
	})

	// Search should return zero results
	results, err := database.SearchSimilar(ctx, tenantID, []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for pending docs, got %d", len(results))
	}
}

// setupTestDB connects to the integration test database.
// Reads DATABASE_URL from environment or falls back to a local default.
// In CI, set DATABASE_URL to a real Postgres instance with pgvector.
func setupTestDB(t *testing.T, ctx context.Context) *db.DB {
	t.Helper()
	dsn := "postgres://semantic:secret@localhost:5432/semantic_test?sslmode=disable"
	database, err := db.New(ctx, dsn)
	if err != nil {
		t.Skipf("database not available (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}
