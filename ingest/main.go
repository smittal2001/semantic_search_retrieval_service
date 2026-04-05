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
	"github.com/yourname/semantic-search/internal/config"
	"github.com/yourname/semantic-search/internal/db"
	"github.com/yourname/semantic-search/internal/models"
	"github.com/yourname/semantic-search/internal/queue"
	pb "github.com/yourname/semantic-search/proto"
)

// ingestServer implements the IngestService gRPC interface.
type ingestServer struct {
	pb.UnimplementedIngestServiceServer
	db    *db.DB
	queue *queue.Queue
}

// IngestDocument is the primary RPC. It:
//  1. Validates the request
//  2. Upserts the document to Postgres (status=pending)
//  3. Publishes an EmbedJob to SQS
//  4. Returns immediately — embedding is async
//
// Order matters: Postgres write MUST succeed before publishing to the queue.
// If the service crashes after the DB write but before the queue publish,
// the document will sit in 'pending' state. An operator can re-trigger
// embedding by re-ingesting or via a backfill script.
func (s *ingestServer) IngestDocument(ctx context.Context, req *pb.IngestRequest) (*pb.IngestResponse, error) {
	tenantID := auth.TenantFromCtx(ctx)

	// Validate
	if req.DocumentId == "" {
		return nil, fmt.Errorf("document_id is required")
	}
	if req.Text == "" || len(req.Text) > 100_000 {
		return nil, fmt.Errorf("text must be between 1 and 100,000 characters")
	}

	// Convert proto metadata map to Go map
	meta := make(map[string]string, len(req.Metadata))
	for k, v := range req.Metadata {
		meta[k] = v
	}

	// Step 1: Write to Postgres
	doc := &models.Document{
		ID:       req.DocumentId,
		TenantID: tenantID,
		Text:     req.Text,
		Metadata: meta,
	}
	if err := s.db.UpsertDocument(ctx, doc); err != nil {
		slog.Error("upsert document failed", "doc_id", req.DocumentId, "err", err)
		return nil, fmt.Errorf("store document: %w", err)
	}

	// Step 2: Publish embed job to SQS
	job := models.EmbedJob{
		DocumentID: req.DocumentId,
		TenantID:   tenantID,
	}
	if err := s.queue.Publish(ctx, job); err != nil {
		// The document exists in Postgres as 'pending'.
		// Backfill script can recover by re-publishing jobs for all pending rows.
		slog.Error("publish embed job failed", "doc_id", req.DocumentId, "err", err)
		return nil, fmt.Errorf("publish embed job: %w", err)
	}

	slog.Info("document ingested", "doc_id", req.DocumentId, "tenant_id", tenantID)
	return &pb.IngestResponse{
		DocumentId: req.DocumentId,
		Status:     "queued",
	}, nil
}

// GetDocument retrieves a document's metadata and current status.
func (s *ingestServer) GetDocument(ctx context.Context, req *pb.GetDocRequest) (*pb.GetDocResponse, error) {
	tenantID := auth.TenantFromCtx(ctx)

	doc, err := s.db.GetDocument(ctx, req.DocumentId, tenantID)
	if err != nil {
		return nil, fmt.Errorf("get document: %w", err)
	}

	return &pb.GetDocResponse{
		DocumentId: doc.ID,
		Text:       doc.Text,
		Metadata:   doc.Metadata,
		Status:     string(doc.Status),
		Model:      doc.Model,
	}, nil
}

// DeleteDocument removes a document. If it has a vector, it is also removed
// from pgvector. The caller should also invalidate any Redis cache for this tenant.
func (s *ingestServer) DeleteDocument(ctx context.Context, req *pb.DeleteDocRequest) (*pb.DeleteDocResponse, error) {
	tenantID := auth.TenantFromCtx(ctx)

	deleted, err := s.db.DeleteDocument(ctx, req.DocumentId, tenantID)
	if err != nil {
		return nil, fmt.Errorf("delete document: %w", err)
	}

	return &pb.DeleteDocResponse{Deleted: deleted}, nil
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

	// Connect to SQS
	q, err := queue.New(ctx, cfg.AWSRegion, cfg.SQSQueueURL)
	if err != nil {
		slog.Error("connect to queue", "err", err)
		os.Exit(1)
	}

	// Set up gRPC server with auth interceptor
	validator := auth.NewValidator(cfg.JWTSecret)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(validator.UnaryInterceptor()),
	)

	pb.RegisterIngestServiceServer(grpcServer, &ingestServer{
		db:    database,
		queue: q,
	})
	reflection.Register(grpcServer) // enables grpcurl in dev

	// Start listening
	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		slog.Error("listen", "addr", cfg.GRPCAddr, "err", err)
		os.Exit(1)
	}

	// Graceful shutdown on SIGTERM / SIGINT
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		slog.Info("shutdown signal received")
		grpcServer.GracefulStop()
		cancel()
	}()

	slog.Info("ingest service listening", "addr", cfg.GRPCAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
