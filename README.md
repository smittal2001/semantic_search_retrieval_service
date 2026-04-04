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
