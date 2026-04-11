package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/yourname/semantic-search/internal/auth"
	"github.com/yourname/semantic-search/internal/config"
	pb "github.com/yourname/semantic-search/proto"
)

// Gateway routes REST requests to gRPC backend services.
// It handles authentication and translates HTTP <-> gRPC on the edge.
type Gateway struct {
	cfg          *config.Config
	validator    *auth.Validator
	ingestClient pb.IngestServiceClient
	searchClient pb.SearchServiceClient
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	// Dial internal gRPC services (insecure — mTLS handled by service mesh in prod)
	ingestConn, err := grpc.NewClient(cfg.IngestServiceAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("dial ingest service", "err", err)
		os.Exit(1)
	}
	defer ingestConn.Close()

	searchConn, err := grpc.NewClient(cfg.SearchServiceAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("dial search service", "err", err)
		os.Exit(1)
	}
	defer searchConn.Close()

	gw := &Gateway{
		cfg:          cfg,
		validator:    auth.NewValidator(cfg.JWTSecret),
		ingestClient: pb.NewIngestServiceClient(ingestConn),
		searchClient: pb.NewSearchServiceClient(searchConn),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", gw.handleHealth)
	mux.HandleFunc("POST /v1/documents", gw.authMiddleware(gw.handleIngest))
	mux.HandleFunc("GET /v1/documents/{id}", gw.authMiddleware(gw.handleGetDocument))
	mux.HandleFunc("DELETE /v1/documents/{id}", gw.authMiddleware(gw.handleDeleteDocument))
	mux.HandleFunc("GET /v1/search", gw.authMiddleware(gw.handleSearch))

	srv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      gw.loggingMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		slog.Info("shutdown signal received")
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	slog.Info("gateway listening", "addr", cfg.HTTPAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

// ── Middleware ────────────────────────────────────────────────────────────────

// authMiddleware validates the Bearer JWT, extracts tenant_id from claims,
// and injects it into the request context. Downstream handlers read it via
// auth.TenantFromCtx — they never trust the caller to supply tenant_id.
func (g *Gateway) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeError(w, http.StatusUnauthorized, "missing Authorization header")
			return
		}
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			writeError(w, http.StatusUnauthorized, "Authorization must be 'Bearer <token>'")
			return
		}
		claims, err := g.validator.ParseToken(parts[1])
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		// Inject tenant_id into context — forwarded to gRPC via metadata
		ctx := context.WithValue(r.Context(), tenantKey{}, claims.TenantID)
		ctx = context.WithValue(ctx, tokenKey{}, parts[1])
		next(w, r.WithContext(ctx))
	}
}

// loggingMiddleware logs every request with method, path, status, and duration.
func (g *Gateway) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// ── Handlers ─────────────────────────────────────────────────────────────────

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (g *Gateway) handleIngest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DocumentID string            `json:"document_id"`
		Text       string            `json:"text"`
		Metadata   map[string]string `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	ctx := grpcCtx(r)
	resp, err := g.ingestClient.IngestDocument(ctx, &pb.IngestRequest{
		DocumentId: body.DocumentID,
		Text:       body.Text,
		Metadata:   body.Metadata,
	})
	if err != nil {
		writeGRPCError(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, resp)
}

func (g *Gateway) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := grpcCtx(r)
	resp, err := g.ingestClient.GetDocument(ctx, &pb.GetDocRequest{DocumentId: id})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (g *Gateway) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := grpcCtx(r)
	resp, err := g.ingestClient.DeleteDocument(ctx, &pb.DeleteDocRequest{DocumentId: id})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (g *Gateway) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "query parameter 'q' is required")
		return
	}
	limit := int32(10)
	ctx := grpcCtx(r)
	resp, err := g.searchClient.Search(ctx, &pb.SearchRequest{
		Query: query,
		Limit: limit,
	})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── helpers ───────────────────────────────────────────────────────────────────

type tenantKey struct{}
type tokenKey struct{}

// grpcCtx builds a context with the JWT forwarded as gRPC metadata.
// The downstream gRPC services re-validate the token and extract tenant_id
// themselves — they never trust the gateway to supply it in plaintext.
func grpcCtx(r *http.Request) context.Context {
	token, _ := r.Context().Value(tokenKey{}).(string)
	md := metadata.Pairs("authorization", "Bearer "+token)
	return metadata.NewOutgoingContext(r.Context(), md)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeGRPCError(w http.ResponseWriter, err error) {
	slog.Error("grpc error", "err", err)
	writeError(w, http.StatusInternalServerError, fmt.Sprintf("upstream error: %v", err))
}

// responseWriter wraps http.ResponseWriter to capture the status code for logging.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
