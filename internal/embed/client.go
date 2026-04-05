package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

const (
	openAIEmbedURL = "https://api.openai.com/v1/embeddings"
	maxAttempts    = 5
	baseDelay      = time.Second
)

// Client calls the embedding API with a token-bucket rate limiter.
// It is safe for concurrent use — all embed worker goroutines share one Client.
type Client struct {
	apiKey  string
	model   string
	dims    int
	apiURL  string
	limiter *rate.Limiter
	http    *http.Client
}

// NewClient creates an embeddings client pointed at the OpenAI API.
func NewClient(apiKey, model string, dims, rpm int) *Client {
	return newClient(openAIEmbedURL, apiKey, model, dims, rpm)
}

// NewClientWithURL creates a client pointed at a custom URL.
// Used in tests to inject a fake server.
func NewClientWithURL(apiURL, apiKey, model string, dims, rpm int) *Client {
	return newClient(apiURL, apiKey, model, dims, rpm)
}

func newClient(apiURL, apiKey, model string, dims, rpm int) *Client {
	perSec := rate.Limit(float64(rpm) / 60.0)
	return &Client{
		apiKey:  apiKey,
		model:   model,
		dims:    dims,
		apiURL:  apiURL,
		limiter: rate.NewLimiter(perSec, 10),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Model returns the model name this client uses.
func (c *Client) Model() string { return c.model }

// Embed converts a single text into a vector.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// EmbedBatch embeds multiple texts in a single API call.
// Returns vectors in the same order as the input texts.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody, err := json.Marshal(embedRequest{
		Model: c.model,
		Input: texts,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limiter: %w", err)
		}

		vecs, retry, err := c.doRequest(ctx, reqBody)
		if err == nil {
			return vecs, nil
		}

		lastErr = err
		if !retry {
			return nil, err
		}

		delay := baseDelay*(1<<attempt) + time.Duration(rand.Int63n(int64(200*time.Millisecond)))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, fmt.Errorf("embed failed after %d attempts: %w", maxAttempts, lastErr)
}

func (c *Client) doRequest(ctx context.Context, body []byte) ([][]float32, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, fmt.Errorf("read body: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		var result embedResponse
		if err := json.Unmarshal(respBytes, &result); err != nil {
			return nil, false, fmt.Errorf("unmarshal response: %w", err)
		}
		vecs := make([][]float32, len(result.Data))
		for _, d := range result.Data {
			if d.Index < len(vecs) {
				vecs[d.Index] = d.Embedding
			}
		}
		return vecs, false, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, true, fmt.Errorf("rate limited (429)")

	case resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("server error %d", resp.StatusCode)

	default:
		return nil, false, fmt.Errorf("non-retryable error %d: %s", resp.StatusCode, string(respBytes))
	}
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}
