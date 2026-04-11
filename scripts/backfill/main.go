// backfill is a one-shot CLI tool that re-publishes SQS embed jobs for any
// document stuck in 'pending' status older than a given age.
//
// When to run:
//   - After an embed worker outage (queue messages expired after 4-day TTL)
//   - After switching to a new embedding model (re-index all documents)
//   - After a bulk import where some documents failed to embed
//
// Usage:
//
//	go run ./scripts/backfill \
//	  --older-than=24h \
//	  --tenant-id=abc123 \
//	  --dry-run=true
//
// The script is idempotent: publishing the same document_id twice within
// 5 minutes is deduplicated by SQS FIFO. Running it multiple times is safe.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/yourname/semantic-search/internal/config"
	"github.com/yourname/semantic-search/internal/models"
	"github.com/yourname/semantic-search/internal/queue"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	olderThan := flag.Duration("older-than", 30*time.Minute,
		"re-queue documents pending for longer than this duration")
	tenantID := flag.String("tenant-id", "",
		"restrict to a specific tenant (empty = all tenants)")
	dryRun := flag.Bool("dry-run", false,
		"print what would be published without actually publishing")
	batchSize := flag.Int("batch-size", 100,
		"number of documents to process per DB query")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("connect to database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	q, err := queue.New(ctx, cfg.AWSRegion, cfg.SQSQueueURL)
	if err != nil {
		slog.Error("connect to queue", "err", err)
		os.Exit(1)
	}

	cutoff := time.Now().Add(-*olderThan)
	slog.Info("starting backfill",
		"older_than", *olderThan,
		"cutoff", cutoff.Format(time.RFC3339),
		"tenant_id", *tenantID,
		"dry_run", *dryRun,
	)

	var (
		total    int
		queued   int
		lastID   string
	)

	for {
		// Paginate using keyset pagination (cheaper than OFFSET at scale)
		query := `
			SELECT id, tenant_id
			FROM documents
			WHERE status     = 'pending'
			  AND created_at < $1
			  AND id         > $2
			  AND ($3 = '' OR tenant_id = $3)
			ORDER BY id
			LIMIT $4
		`
		rows, err := pool.Query(ctx, query, cutoff, lastID, *tenantID, *batchSize)
		if err != nil {
			slog.Error("query pending documents", "err", err)
			os.Exit(1)
		}

		var batch []models.EmbedJob
		for rows.Next() {
			var job models.EmbedJob
			if err := rows.Scan(&job.DocumentID, &job.TenantID); err != nil {
				slog.Error("scan row", "err", err)
				os.Exit(1)
			}
			batch = append(batch, job)
			lastID = job.DocumentID
		}
		rows.Close()

		if len(batch) == 0 {
			break // no more pages
		}

		total += len(batch)

		for _, job := range batch {
			if *dryRun {
				fmt.Printf("[dry-run] would queue doc_id=%s tenant_id=%s\n", job.DocumentID, job.TenantID)
				continue
			}
			if err := q.Publish(ctx, job); err != nil {
				slog.Error("publish job", "doc_id", job.DocumentID, "err", err)
				// Continue — don't abort the whole backfill for one failure
				continue
			}
			queued++
			slog.Info("queued embed job", "doc_id", job.DocumentID, "tenant_id", job.TenantID)
		}
	}

	slog.Info("backfill complete",
		"total_pending", total,
		"queued", queued,
		"dry_run", *dryRun,
	)
}
