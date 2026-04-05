package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/smittal2001/semantic-search/internal/models"
)

// Queue wraps the AWS SQS client with typed publish/receive methods.
type Queue struct {
	client   *sqs.Client
	queueURL string
}

// New creates a Queue using the default AWS credential chain.
func New(ctx context.Context, region, queueURL string) (*Queue, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &Queue{
		client:   sqs.NewFromConfig(cfg),
		queueURL: queueURL,
	}, nil
}

// Publish sends an EmbedJob to the SQS FIFO queue.
//
// MessageDeduplicationId = document_id:
//   Re-ingesting the same document_id within the 5-minute dedup window
//   will NOT create a duplicate embed job.
//
// MessageGroupId = tenant_id:
//   Messages for the same tenant are delivered in FIFO order.
func (q *Queue) Publish(ctx context.Context, job models.EmbedJob) error {
	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	_, err = q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(q.queueURL),
		MessageBody:            aws.String(string(body)),
		MessageDeduplicationId: aws.String(job.DocumentID), // idempotency
		MessageGroupId:         aws.String(job.TenantID),   // per-tenant ordering
	})
	if err != nil {
		return fmt.Errorf("sqs send: %w", err)
	}
	return nil
}

// Receive long-polls SQS for up to maxMessages (max 10).
// waitSecs controls the long-poll duration (max 20).
// Blocks until at least one message arrives OR waitSecs elapses.
func (q *Queue) Receive(ctx context.Context, maxMessages int32, waitSecs int32) ([]types.Message, error) {
	out, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(q.queueURL),
		MaxNumberOfMessages: maxMessages,
		WaitTimeSeconds:     waitSecs,         // long-poll: no busy-loop
		AttributeNames:      []types.QueueAttributeName{"All"},
	})
	if err != nil {
		return nil, fmt.Errorf("sqs receive: %w", err)
	}
	return out.Messages, nil
}

// Delete removes a message from the queue (ACK).
// Must be called AFTER the message has been fully processed and the result
// written to the database. Calling it before risks data loss on crash.
func (q *Queue) Delete(ctx context.Context, receiptHandle string) error {
	_, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(q.queueURL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	if err != nil {
		return fmt.Errorf("sqs delete: %w", err)
	}
	return nil
}

// ParseJob deserialises an SQS message body into an EmbedJob.
func ParseJob(msg types.Message) (models.EmbedJob, error) {
	var job models.EmbedJob
	if err := json.Unmarshal([]byte(*msg.Body), &job); err != nil {
		return job, fmt.Errorf("parse embed job: %w", err)
	}
	return job, nil
}
