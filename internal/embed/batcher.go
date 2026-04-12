package embed

import (
    "context"
    "sync"
    "time"
)

type batchItem struct {
    text   string
    result chan batchResult
}

type batchResult struct {
    vector []float32
    err    error
}

type Batcher struct {
    client      *Client
    mu          sync.Mutex
    pending     []batchItem
    timer       *time.Timer
    window      time.Duration  // e.g. 50ms
    maxSize     int            // e.g. 100
}

func NewBatcher(client *Client, window time.Duration, maxSize int) *Batcher {
    return &Batcher{
        client:  client,
        window:  window,
        maxSize: maxSize,
        pending: make([]batchItem, 0, maxSize),
    }
}

// Add is called by each goroutine. It drops the text into the shared
// bucket and then BLOCKS on its personal result channel until flush
// sends a vector back.
func (b *Batcher) Add(ctx context.Context, text string) ([]float32, error) {
    // Each call to Add creates a batchItem with a private result channel (batchResult) 
	// that this goroutine will read from after flush() processes the batch.
	result := make(chan batchResult, 1)  // this goroutine's private mailbox

    b.mu.Lock()
    b.pending = append(b.pending, batchItem{text: text, result: result})

    shouldFlush := len(b.pending) >= b.maxSize
    if len(b.pending) == 1 {
        // First item in batch starts the timer. 
		// Subsequent items just append to the bucket.
		// If the batch fills up before the timer expires, it flushes immediately.
        b.timer = time.AfterFunc(b.window, b.flush)
    }
    b.mu.Unlock()

    if shouldFlush {
        b.timer.Stop()
        b.flush()
    }

    // Block here until flush() sends our vector back
    r := <-result
    return r.vector, r.err
}

func (b *Batcher) flush() {
    b.mu.Lock()
    items := b.pending
    b.pending = make([]batchItem, 0, b.maxSize)  // fresh bucket immediately
    b.mu.Unlock()

    if len(items) == 0 {
        return
    }

    // Creates an array of texts to send to the embedding API in one batch call
    texts := make([]string, len(items))
    for i, item := range items {
        texts[i] = item.text
    }

    vectors, err := b.client.EmbedBatch(context.Background(), texts)

    // Fan results back each goroutine unblocks when it receives its vector
    for i, item := range items {
        if err != nil {
            item.result <- batchResult{err: err}
        } else {
            item.result <- batchResult{vector: vectors[i]}
        }
    }
}