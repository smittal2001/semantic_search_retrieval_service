package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
// Each service reads only the fields it needs; unused fields are ignored.
type Config struct {
	// ── Server ──────────────────────────────────────────────────────────────
	GRPCAddr string 
	HTTPAddr string 

	// ── Postgres ────────────────────────────────────────────────────────────
	DatabaseURL string 

	// ── Redis ───────────────────────────────────────────────────────────────
	RedisAddr     string
	RedisPassword string
	CacheTTL      time.Duration

	// ── SQS ─────────────────────────────────────────────────────────────────
	SQSQueueURL string
	AWSRegion   string

	// ── Embed API ────────────────────────────────────────────────────────────
	OpenAIAPIKey  string
	EmbedModel    string // "text-embedding-3-small"
	EmbedDims     int    
	EmbedBatchMax int    // max texts per API call

	// ── Worker ──────────────────────────────────────────────────────────────
	WorkerConcurrency int           // goroutines per pod
	WorkerPollWait    int32         // SQS long-poll seconds (max 20)
	WorkerVisibility  int32         // SQS visibility timeout seconds
	ShutdownTimeout   time.Duration

	// ── Auth ─────────────────────────────────────────────────────────────────
	JWTSecret string

	// ── Internal service addresses ───────────────────────────────────────────
	IngestServiceAddr string 
	SearchServiceAddr string
}

// Load reads configuration from environment variables, returning an error if
// any required variable is missing.
func Load() (*Config, error) {
	c := &Config{
		GRPCAddr:          envOr("GRPC_ADDR", ":9090"),
		HTTPAddr:          envOr("HTTP_ADDR", ":8080"),
		DatabaseURL:       mustEnv("DATABASE_URL"),
		RedisAddr:         envOr("REDIS_ADDR", "localhost:6379"),
		RedisPassword:     os.Getenv("REDIS_PASSWORD"),
		SQSQueueURL:       mustEnv("SQS_QUEUE_URL"),
		AWSRegion:         envOr("AWS_REGION", "us-east-1"),
		OpenAIAPIKey:      mustEnv("OPENAI_API_KEY"),
		EmbedModel:        envOr("EMBED_MODEL", "text-embedding-3-small"),
		EmbedDims:         envIntOr("EMBED_DIMS", 1536),
		EmbedBatchMax:     envIntOr("EMBED_BATCH_MAX", 100),
		WorkerConcurrency: envIntOr("WORKER_CONCURRENCY", 10),
		WorkerPollWait:    int32(envIntOr("WORKER_POLL_WAIT", 20)),
		WorkerVisibility:  int32(envIntOr("WORKER_VISIBILITY", 30)),
		ShutdownTimeout:   envDurationOr("SHUTDOWN_TIMEOUT", 30*time.Second),
		CacheTTL:          envDurationOr("CACHE_TTL", 120*time.Second),
		JWTSecret:         mustEnv("JWT_SECRET"),
		IngestServiceAddr: envOr("INGEST_SERVICE_ADDR", "ingest-service:9090"),
		SearchServiceAddr: envOr("SEARCH_SERVICE_ADDR", "search-service:9090"),
	}
	return c, nil
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("required environment variable %q is not set", key))
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
