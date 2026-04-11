package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/yourname/semantic-search/internal/auth"
	"github.com/yourname/semantic-search/internal/cache"
	"github.com/yourname/semantic-search/internal/config"
	"github.com/yourname/semantic-search/internal/db"
	"github.com/yourname/semantic-search/internal/embed"
	pb "github.com/yourname/semantic-search/proto"
)

const (
	defaultLimit = 10
	maxLimit     = 100
)

// searchServer implements the SearchService gRPC interface.
type searchServer struct {
	pb.UnimplementedSearchServiceServer
	db          *db.DB
	cache       *cache.Cache
	embedClient *embed.Client
}

// Search is the core search RPC. The flow is:
//
//  1. Embed the query string using the SAME model as ingestion
//     (mismatched models produce meaningless cosine distances)
//  2. Check Redis cache — return immediately on hit
//  3. Run ANN cosine similarity search in pgvector
//  4. Cache the results for subsequent identical queries
//  5. Return ranked results to caller
//
// Redis is NOT on the critical path — if it is unavailable, the search
// falls through to Postgres silently.
func (s *searchServer) Search(ctx context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	tenantID := auth.TenantFromCtx(ctx)

	if req.Query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}

	limit := int(req.Limit)
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// ── Step 1: Embed the query ────────────────────────────────────────────
	// The embed call is on the hot path — it adds ~50-200ms of latency.
	// Cache hit (step 2) skips this; in practice most queries are unique.
	queryVec, err := s.embedClient.Embed(ctx, req.Query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	// ── Step 2: Cache lookup ───────────────────────────────────────────────
	// Key = hash(tenantID + queryVec). Same text always produces the same
	// vector (for the same model), so this is a stable cache key.
	if cached, hit, _ := s.cache.GetResults(ctx, tenantID, queryVec); hit {
		slog.Debug("cache hit", "tenant_id", tenantID)
		return buildResponse(cached), nil
	}

	// ── Step 3: ANN search via pgvector ───────────────────────────────────
	// The <=> cosine distance operator is accelerated by the HNSW index.
	// tenant_id filter is applied inside SearchSimilar — cross-tenant
	// leakage is structurally impossible.
	results, err := s.db.SearchSimilar(ctx, tenantID, queryVec, limit)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}

	// ── Step 4: Cache results ─────────────────────────────────────────────
	// Best-effort — a cache write failure does not fail the search.
	if err := s.cache.SetResults(ctx, tenantID, queryVec, results); err != nil {
		slog.Warn("cache set failed", "err", err)
	}

	slog.Info("search completed",
		"tenant_id", tenantID,
		"query_len", len(req.Query),
		"results", len(results),
	)

	return buildResponse(results), nil
}

func buildResponse(results []SearchResult) *pb.SearchResponse {
	pbResults := make([]*pb.SearchResult, len(results))
	for i, r := range results {
		pbResults[i] = &pb.SearchResult{
			DocumentId: r.DocumentID,
			Text:       r.Text,
			Metadata:   r.Metadata,
			Similarity: r.Similarity,
		}
	}
	return &pb.SearchResponse{Results: pbResults}
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect to Postgres
	database, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("connect to database", "err", err)
		os.Exit(1)
	}
	defer database.Close()

	// Connect to Redis
	redisCache := cache.New(cfg.RedisAddr, cfg.RedisPassword, cfg.CacheTTL)
	defer redisCache.Close()

	// Verify Redis is reachable (non-fatal — search works without it)
	if err := redisCache.Ping(ctx); err != nil {
		slog.Warn("redis not reachable — cache disabled", "err", err)
	}

	// Create embed client
	embedClient := embed.NewClient(cfg.OpenAIAPIKey, cfg.EmbedModel, cfg.EmbedDims, 3000)

	// Set up gRPC server
	validator := auth.NewValidator(cfg.JWTSecret)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(validator.UnaryInterceptor()),
	)

	pb.RegisterSearchServiceServer(grpcServer, &searchServer{
		db:          database,
		cache:       redisCache,
		embedClient: embedClient,
	})
	reflection.Register(grpcServer)

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		slog.Error("listen", "addr", cfg.GRPCAddr, "err", err)
		os.Exit(1)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		slog.Info("shutdown signal received")
		grpcServer.GracefulStop()
		cancel()
	}()

	slog.Info("search service listening", "addr", cfg.GRPCAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
