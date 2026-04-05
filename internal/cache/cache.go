package cache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/smittal2001/semantic-search/internal/models"
)

// Cache wraps Redis and provides typed get/set for search results.
type Cache struct {
	client *redis.Client
	ttl    time.Duration
}

// New creates a Cache backed by Redis.
func New(addr, password string, ttl time.Duration) *Cache {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       0,
	})
	return &Cache{client: client, ttl: ttl}
}

// Close releases the Redis connection.
func (c *Cache) Close() error { return c.client.Close() }

// GetResults retrieves cached search results.
// Returns (results, true, nil) on hit, (nil, false, nil) on miss.
func (c *Cache) GetResults(ctx context.Context, tenantID string, queryVec []float32) ([]models.SearchResult, bool, error) {
	key := cacheKey(tenantID, queryVec)
	val, err := c.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil // cache miss — not an error
	}
	if err != nil {
		// Redis errors are non-fatal: fall through to Postgres.
		return nil, false, nil
	}
	var results []models.SearchResult
	if err := json.Unmarshal(val, &results); err != nil {
		return nil, false, fmt.Errorf("unmarshal cached results: %w", err)
	}
	return results, true, nil
}

// SetResults stores search results with the configured TTL.
// Errors are logged but not returned — caching is best-effort.
func (c *Cache) SetResults(ctx context.Context, tenantID string, queryVec []float32, results []models.SearchResult) error {
	key := cacheKey(tenantID, queryVec)
	b, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("marshal results: %w", err)
	}
	return c.client.Set(ctx, key, b, c.ttl).Err()
}

// InvalidateTenant removes all cached results for a tenant.
// Called when documents are deleted or re-indexed.
func (c *Cache) InvalidateTenant(ctx context.Context, tenantID string) error {
	pattern := fmt.Sprintf("search:%s:*", tenantID)
	keys, err := c.client.Keys(ctx, pattern).Result()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	return c.client.Del(ctx, keys...).Err()
}

// Ping checks Redis connectivity.
func (c *Cache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

// cacheKey hashes the tenant ID and query vector to a stable Redis key.
// Key format: "search:{tenantID}:{sha256(vector bytes)}"
//
// We hash the vector rather than the query text because:
//   - Two texts with the same embedding produce the same cache key (correct)
//   - Unicode normalisation or whitespace differences don't create cache misses
func cacheKey(tenantID string, vec []float32) string {
	h := sha256.New()
	for _, f := range vec {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, *(*uint32)((*[4]byte)(&[4]byte{byte(f), byte(f >> 8), byte(f >> 16), byte(f >> 24)})))
		h.Write(b)
	}
	return fmt.Sprintf("search:%s:%x", tenantID, h.Sum(nil))
}
