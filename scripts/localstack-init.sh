#!/bin/sh
# Runs inside LocalStack on startup to create the SQS FIFO queue.
# LocalStack simulates AWS SQS locally — no real AWS account needed.

echo "Creating SQS FIFO queue..."

awslocal sqs create-queue \
  --queue-name embed-jobs.fifo \
  --attributes '{
    "FifoQueue":                    "true",
    "ContentBasedDeduplication":    "false",
    "VisibilityTimeout":            "30",
    "MessageRetentionPeriod":       "345600",
    "ReceiveMessageWaitTimeSeconds": "20"
  }'

echo "Queue created: embed-jobs.fifo"
