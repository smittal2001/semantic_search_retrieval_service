package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smittal2001/semantic-search/internal/embed"
)

// fakeEmbedServer returns a test HTTP server that mimics the OpenAI embed API.
// statusCode controls what it returns. After firstFailCount failures it returns 200.
func fakeEmbedServer(t *testing.T, firstFailCount int, failStatus int) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if n <= firstFailCount {
			w.WriteHeader(failStatus)
			return
		}
		// Parse the request to know how many vectors to return
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		type dataItem struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]dataItem, len(req.Input))
		for i := range req.Input {
			vec := make([]float32, 4) // short vectors for tests
			vec[0] = float32(i) + 0.1
			items[i] = dataItem{Embedding: vec, Index: i}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
}

// TestEmbed_Success verifies a clean request returns vectors.
func TestEmbed_Success(t *testing.T) {
	srv := fakeEmbedServer(t, 0, 0)
	defer srv.Close()

	client := embed.NewClientWithURL(srv.URL, "test-key", "test-model", 4, 3000)
	vec, err := client.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(vec) != 4 {
		t.Fatalf("expected 4 dims, got %d", len(vec))
	}
}

// TestEmbed_RetryOn429 verifies that a 429 response triggers a retry and
// eventually succeeds.
func TestEmbed_RetryOn429(t *testing.T) {
	// Fail twice with 429 then succeed
	srv := fakeEmbedServer(t, 2, http.StatusTooManyRequests)
	defer srv.Close()

	client := embed.NewClientWithURL(srv.URL, "test-key", "test-model", 4, 3000)
	vec, err := client.Embed(context.Background(), "retry me")
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if len(vec) == 0 {
		t.Fatal("expected non-empty vector")
	}
}

// TestEmbed_RetryOn500 verifies server errors are also retried.
func TestEmbed_RetryOn500(t *testing.T) {
	srv := fakeEmbedServer(t, 1, http.StatusInternalServerError)
	defer srv.Close()

	client := embed.NewClientWithURL(srv.URL, "test-key", "test-model", 4, 3000)
	_, err := client.Embed(context.Background(), "server error")
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
}

// TestEmbed_ContextCancellation verifies that cancelling the context stops retries.
func TestEmbed_ContextCancellation(t *testing.T) {
	// Always fail — context cancellation should stop us before exhausting retries
	srv := fakeEmbedServer(t, 100, http.StatusTooManyRequests)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	client := embed.NewClientWithURL(srv.URL, "test-key", "test-model", 4, 3000)
	_, err := client.Embed(ctx, "should be cancelled")
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

// TestEmbedBatch_OrderPreserved verifies that EmbedBatch returns vectors
// in the same order as the input texts, even if the server returns them
// out of order (which OpenAI does not do but is a good invariant to test).
func TestEmbedBatch_OrderPreserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		// Return vectors in REVERSE order to test our sorting
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, len(req.Input))
		for i := range req.Input {
			items[len(req.Input)-1-i] = item{
				Embedding: []float32{float32(i)},
				Index:     i,
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer srv.Close()

	client := embed.NewClientWithURL(srv.URL, "key", "model", 1, 3000)
	texts := []string{"zero", "one", "two", "three"}
	vecs, err := client.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("expected %d vectors, got %d", len(texts), len(vecs))
	}
	// Vector at index i should have first element == float32(i)
	for i, vec := range vecs {
		if vec[0] != float32(i) {
			t.Errorf("index %d: expected vec[0]=%v, got %v", i, float32(i), vec[0])
		}
	}
}

// TestEmbed_NonRetryable verifies that a 400 error is not retried.
func TestEmbed_NonRetryable(t *testing.T) {
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid input"}`))
	}))
	defer srv.Close()

	client := embed.NewClientWithURL(srv.URL, "key", "model", 4, 3000)
	_, err := client.Embed(context.Background(), "bad input")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	// Should have been called exactly once — no retries on 400
	if n := int(callCount.Load()); n != 1 {
		t.Errorf("expected 1 call, got %d (should not retry 400)", n)
	}
}
