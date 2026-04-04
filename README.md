# semantic_search_retrieval_service
Semantic search and retrieval service built in Go. The system accepts documents via API, asynchronously converts them into vector embeddings using an LLM embedding API, stores those vectors in Postgres with pgvector, and exposes a fast semantic search endpoint that returns documents ranked by meaning not just keyword match.
