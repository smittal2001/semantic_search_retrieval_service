package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourname/semantic-search/internal/config"
	"github.com/yourname/semantic-search/internal/db"
	"github.com/yourname/semantic-search/internal/embed"
	"github.com/yourname/semantic-search/internal/queue"
)

// Worker holds the dependencies for the embed consumer loop.
type Worker struct {
	db          *db.DB
	queue       *queue.Queue
	embedClient *embed.Client
	cfg         *config.Config
}

// Run starts the long-poll consumer loop. It blocks until ctx is cancelled,
// then drains all in-flight goroutines before returning.
//
// The semaphore pattern used here is idiomatic Go for bounded concurrency:
//   - sem <- struct{}{} claims a slot (blocks if at capacity)
//   - <-sem releases a slot
//   - Filling the channel to capacity during shutdown waits for all goroutines
func (w *Worker) Run(ctx context.Context) {
	sem := make(chan struct{}, w.cfg.WorkerConcurrency)

	slog.Info("embed worker started", "concurrency", w.cfg.WorkerConcurrency)

	for {
		// ── Graceful shutdown ──────────────────────────────────────────────
		select {
		case <-ctx.Done():
			slog.Info("shutdown: draining in-flight goroutines")
			// Fill the semaphore to capacity — blocks until every goroutine
			// releases its slot by returning from process().
			for i := 0; i < cap(sem); i++ {
				sem <- struct{}{}
			}
			slog.Info("embed worker stopped cleanly")
			return
		default:
		}

		// ── Long-poll SQS ─────────────────────────────────────────────────
		// WaitTimeSeconds=20 makes ReceiveMessage block up to 20s when the
		// queue is empty. Without this, the loop becomes a hot busy-loop
		// that hammers SQS thousands of times per second.
		messages, err := w.queue.Receive(ctx, 10, w.cfg.WorkerPollWait)
		if err != nil {
			if ctx.Err() != nil {
				return // context cancelled — normal shutdown path
			}
			slog.Error("sqs receive failed, backing off", "err", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// ── Dispatch each message to a bounded goroutine ──────────────────
		for _, msg := range messages {
			// Acquire a semaphore slot.
			// Blocks if w.cfg.WorkerConcurrency goroutines are already running.
			sem <- struct{}{}

			// IMPORTANT: copy msg into the goroutine parameter.
			// Without this, all goroutines would capture the same loop variable
			// and process the last message in the batch repeatedly.
			go func(m interface{ GetBody() *string; GetReceiptHandle() *string }) {
				defer func() { <-sem }() // always release slot on exit
				w.process(ctx, m)
			}(sqsMessage{msg})
		}
	}
}

// sqsMessage is a thin wrapper so we can pass the SQS message to the goroutine
// without importing the SQS types directly in the closure.
type sqsMessage struct {
	inner interface {
		GetBody() *string
		GetReceiptHandle() *string
	}
}

// process handles a single SQS message:
//  1. Parse the EmbedJob from the message body
//  2. Fetch the document text from Postgres
//  3. Call the embedding API (with retry + rate limiting)
//  4. Write the vector to pgvector and flip status to 'indexed'
//  5. ACK the message (DeleteMessage) — ONLY after a successful write
//
// If any step fails without exhausting retries, the function returns without
// ACKing. The message becomes visible again after the SQS visibility timeout
// and will be redelivered. Processing is idempotent (upsert), so retries
// are safe.
func (w *Worker) process(ctx context.Context, m sqsMessage) {
	// ── Parse ──────────────────────────────────────────────────────────────
	job, err := queue.ParseJob(*(m.inner))
	if err != nil {
		slog.Error("parse job failed — poisoning message", "err", err)
		// Bad message format will never succeed; delete it immediately
		// to avoid it blocking the queue forever.
		_ = w.queue.Delete(ctx, *m.inner.GetReceiptHandle())
		return
	}

	slog.Info("processing embed job", "doc_id", job.DocumentID)

	// ── Fetch text ─────────────────────────────────────────────────────────
	text, err := w.db.GetDocumentText(ctx, job.DocumentID)
	if err != nil {
		slog.Warn("document not found — deleting job", "doc_id", job.DocumentID, "err", err)
		// Document was deleted after the job was queued; safe to discard.
		_ = w.queue.Delete(ctx, *m.inner.GetReceiptHandle())
		return
	}

	// ── Embed ──────────────────────────────────────────────────────────────
	// embedClient handles rate limiting (token bucket) and exponential backoff
	// on 429 / 5xx responses. If all retries are exhausted, it returns an error
	// and the SQS message is NOT acknowledged — it will be retried.
	vector, err := w.embedClient.Embed(ctx, text)
	if err != nil {
		slog.Error("embed failed, leaving message in queue for retry",
			"doc_id", job.DocumentID, "err", err)
		return // do NOT ACK — let SQS redeliver after visibility timeout
	}

	// ── Write vector ───────────────────────────────────────────────────────
	// This is an atomic UPDATE that sets embedding + status = 'indexed'.
	// If this succeeds but the DeleteMessage call below crashes, the message
	// redelivers and we run SetEmbedding again — safe because it's an upsert.
	if err := w.db.SetEmbedding(ctx, job.DocumentID, vector, w.embedClient.Model()); err != nil {
		slog.Error("write embedding failed, leaving message in queue",
			"doc_id", job.DocumentID, "err", err)
		return // do NOT ACK
	}

	// ── ACK (delete from SQS) ──────────────────────────────────────────────
	// Only reached if the DB write succeeded. This is the contract:
	//   write first → ack second
	// Reversing the order risks losing a document if the process crashes
	// between the ACK and the DB write.
	if err := w.queue.Delete(ctx, *m.inner.GetReceiptHandle()); err != nil {
		// The message will redeliver and be processed again.
		// SetEmbedding is idempotent, so this is safe.
		slog.Warn("delete message failed (will redeliver)",
			"doc_id", job.DocumentID, "err", err)
	}

	slog.Info("document indexed", "doc_id", job.DocumentID, "model", w.embedClient.Model())
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

	// Create embed client (3000 RPM = OpenAI tier-1 default)
	embedClient := embed.NewClient(cfg.OpenAIAPIKey, cfg.EmbedModel, cfg.EmbedDims, 3000)

	worker := &Worker{
		db:          database,
		queue:       q,
		embedClient: embedClient,
		cfg:         cfg,
	}

	// Listen for shutdown signals
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		slog.Info("shutdown signal received")
		cancel() // triggers graceful drain in worker.Run()
	}()

	// Blocks until context is cancelled and all goroutines are drained
	worker.Run(ctx)
}
