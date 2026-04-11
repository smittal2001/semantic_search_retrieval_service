### semantic_search_retrieval_service

## Overview
Semantic search and retrieval service built in Go. The system accepts documents via API, asynchronously converts them into vector embeddings using an LLM embedding API, stores those vectors in Postgres with pgvector, and exposes a fast semantic search endpoint that returns documents ranked by meaning not just keyword match.

## High level Architecture

### Entry Point
gRPC + REST gateway — dual protocol, JWT auth middleware, rate limiting. Hits the gRPC and service-to-service communication bullet directly.

### Ingest service 
Validates and persists document metadata to Postgres, publishes an embed job event to SQS

### Embed worker
Consumes SQS messages, calls the embedding API, writes vectors to pgvector, acknowledges messages

### Search service
Embeds the query, runs Approximate Nearest Neighbor (ANN) search against pgvector, fetches metadata, checks Redis cache, returns ranked results

### Storage layer 
Postgres for metadata, pgvector or Qdrant for vectors, Redis for caching. Covers the database/storage bullet cleanly.


### Two types of requests: Ingestion and Search Request 
Ingestion path
Client  →  Gateway (auth + rate limit)  →  Ingest Service (Postgres write)  →  SQS (publish)  →  200 OK returned to client
Asynchronously: SQS  →  Embed Worker (embed API + pgvector write)  →  message ACK

Search path
Client  →  Gateway  →  Search Service  →  Redis (cache check)  →  pgvector ANN query  →  Postgres (metadata fetch)  →  ranked results


## Architecture

```
Client
  │
  ▼
┌─────────────┐   REST/gRPC   ┌────────────────┐
│   Gateway   │──────────────▶│ Ingest Service │──▶ Postgres (metadata)
│  :8080      │               │    :9090       │──▶ SQS FIFO Queue
└─────────────┘               └────────────────┘
  │                                                        │
  │ REST/gRPC   ┌────────────────┐                        ▼
  └────────────▶│ Search Service │         ┌──────────────────────┐
                │    :9090       │         │    Embed Worker      │
                └────────────────┘         │  (SQS consumer)      │
                  │         │              │  calls OpenAI API    │
                  ▼         ▼              │  writes to pgvector  │
               Redis     pgvector         └──────────────────────┘
             (cache)   (ANN index)
```

## Quick Start

### Prerequisites
- Docker & Docker Compose
- An OpenAI API key

### Run locally

```bash
# Clone and start everything
git clone https://github.com/yourname/semantic-search
cd semantic-search

OPENAI_API_KEY=sk-... docker compose up

# Wait ~15s for health checks to pass, then generate a dev JWT
JWT=$(JWT_SECRET=dev-secret-change-in-prod go run ./scripts/gen-jwt)

# Ingest a document
curl -X POST http://localhost:8080/v1/documents \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{
    "document_id": "doc-1",
    "text": "The quick brown fox jumps over the lazy dog",
    "metadata": {"source": "example"}
  }'

# Wait ~2s for the worker to embed it, then search
curl "http://localhost:8080/v1/search?q=fast+animal+leaping" \
  -H "Authorization: Bearer $JWT" | jq .
```

## API Reference

### Ingest a document
```
POST /v1/documents
Authorization: Bearer <jwt>
Content-Type: application/json

{
  "document_id": "unique-id",       // idempotency key — re-posting is safe
  "text": "document content",
  "metadata": {"key": "value"}      // arbitrary string key-value pairs
}
```

Response: `202 Accepted`
```json
{ "document_id": "unique-id", "status": "queued" }
```

### Get a document
```
GET /v1/documents/{id}
Authorization: Bearer <jwt>
```

### Delete a document
```
DELETE /v1/documents/{id}
Authorization: Bearer <jwt>
```

### Search
```
GET /v1/search?q=your+query+here&limit=10
Authorization: Bearer <jwt>
```

Response:
```json
{
  "results": [
    {
      "document_id": "doc-1",
      "text": "The quick brown fox...",
      "metadata": {"source": "example"},
      "similarity": 0.87
    }
  ]
}
```

## Development

```bash
# Run unit tests
make test-unit

# Run integration tests (requires Docker)
make test-integration

# Lint
make lint

# Regenerate protobuf Go code
make proto

# Tail worker logs
make logs-worker

# Re-queue stuck pending documents
make backfill-dry    # preview
make backfill-run    # execute
```

## Configuration

All services are configured via environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `DATABASE_URL` | Postgres connection string | required |
| `SQS_QUEUE_URL` | SQS FIFO queue URL | required |
| `OPENAI_API_KEY` | OpenAI API key | required |
| `JWT_SECRET` | HMAC secret for JWT signing | required |
| `REDIS_ADDR` | Redis address | `localhost:6379` |
| `AWS_REGION` | AWS region | `us-east-1` |
| `EMBED_MODEL` | Embedding model name | `text-embedding-3-small` |
| `EMBED_DIMS` | Embedding dimensions | `1536` |
| `WORKER_CONCURRENCY` | Goroutines per worker pod | `10` |
| `CACHE_TTL` | Redis cache TTL | `120s` |

## Kubernetes Deployment

```bash
# Apply base manifests
kubectl apply -k k8s/base

# Apply production overlay (scaled replicas, prod image tags)
kubectl apply -k k8s/overlays/prod

# Check pod status
make k8s-status
```

The worker deployment uses [KEDA](https://keda.sh) to autoscale based on SQS queue depth:
- Scales to **zero** when the queue is empty (no idle cost)
- Scales up to **20 pods** at peak load
- One pod per 50 queued messages

## Observability

Alert on these signals:

| Signal | Alert threshold | Meaning |
|--------|----------------|---------|
| SQS queue depth | > 500 for 5 min | Worker is down or falling behind |
| `pending` docs older than 10 min | > 0 | Worker outage — run backfill after recovery |
| Search p99 latency | > 200ms | Index fragmentation or Postgres pressure |
| Embed API error rate | > 5% | OpenAI degradation — check status page |