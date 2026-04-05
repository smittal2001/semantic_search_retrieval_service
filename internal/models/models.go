package models

import (
	"time"
)

// DocumentStatus represents the lifecycle state of a document.
type DocumentStatus string

const (
	StatusPending DocumentStatus = "pending" // written to DB, not yet embedded
	StatusIndexed DocumentStatus = "indexed" // vector written, searchable
	StatusFailed  DocumentStatus = "failed"  // embed permanently failed
)

// Document is the core domain object stored in Postgres.
type Document struct {
	ID        string            `json:"id"`
	TenantID  string            `json:"tenant_id"`
	Text      string            `json:"text"`
	Metadata  map[string]string `json:"metadata"`
	Embedding []float32         `json:"embedding,omitempty"`
	Status    DocumentStatus    `json:"status"`
	Model     string            `json:"model,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// EmbedJob is the message published to SQS.
// Deliberately minimal — the worker fetches the full document from Postgres.
type EmbedJob struct {
	DocumentID string `json:"document_id"`
	TenantID   string `json:"tenant_id"`
}

// SearchResult is a ranked document returned by the search service.
type SearchResult struct {
	DocumentID string            `json:"document_id"`
	Text       string            `json:"text"`
	Metadata   map[string]string `json:"metadata"`
	Similarity float32           `json:"similarity"` // cosine similarity 0.0–1.0
}

// ContextKey is used to safely attach values to context.Context.
type ContextKey string

const (
	// ContextKeyTenantID is injected by the auth middleware and read by all services.
	// Services must NEVER trust a caller-supplied tenant_id ALWAYS use this.
	ContextKeyTenantID ContextKey = "tenant_id"
)
