// Package sqsconsumer adapta SQS aos mesmos casos de uso utilizados por HTTP.
package sqsconsumer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const (
	EventQueueName          = "wallet-events.fifo"
	EventDLQName            = "wallet-events-dlq.fifo"
	QueueName               = "wager-transactions.fifo"
	DLQName                 = "wager-transactions-dlq.fifo"
	ConsumerName            = "wager-transactions-v1"
	VisibilitySeconds int32 = 60
	MaxReceives             = 5
)

func NewClient(ctx context.Context, region, endpoint string) (*sqs.Client, error) {
	if strings.TrimSpace(region) == "" {
		return nil, errors.New("AWS_REGION is required")
	}
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, errors.New("invalid SQS endpoint")
		}
	}
	config, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region),
		awsconfig.WithHTTPClient(&http.Client{Timeout: 15 * time.Second, Transport: http.DefaultTransport.(*http.Transport).Clone()}), awsconfig.WithRetryMaxAttempts(2))
	if err != nil {
		return nil, errors.New("could not load AWS configuration")
	}
	return sqs.NewFromConfig(config, func(options *sqs.Options) {
		if endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
	}), nil
}

// Provision é executado pelo setup, não pelo consumidor. Em AWS, use uma
// identidade de provisionamento distinta da role de consumo.
func Provision(ctx context.Context, client *sqs.Client) error {
	if err := provisionFIFO(ctx, client, QueueName, DLQName); err != nil {
		return err
	}
	return provisionFIFO(ctx, client, EventQueueName, EventDLQName)
}
func provisionFIFO(ctx context.Context, client *sqs.Client, queueName, dlqName string) error {
	dlq, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(dlqName), Attributes: map[string]string{
		"FifoQueue": "true", "ContentBasedDeduplication": "false", "MessageRetentionPeriod": "1209600",
	}})
	if err != nil {
		return err
	}
	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: dlq.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if err != nil {
		return err
	}
	arn := attrs.Attributes["QueueArn"]
	if arn == "" {
		return errors.New("DLQ ARN is missing")
	}
	policy, _ := json.Marshal(map[string]any{"deadLetterTargetArn": arn, "maxReceiveCount": "5"})
	source, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(queueName), Attributes: map[string]string{
		"FifoQueue": "true", "ContentBasedDeduplication": "false", "VisibilityTimeout": "60", "ReceiveMessageWaitTimeSeconds": "10",
		"MessageRetentionPeriod": "345600", "RedrivePolicy": string(policy),
	}})
	if err != nil {
		return err
	}
	sourceAttrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: source.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if err != nil {
		return err
	}
	sourceARN := sourceAttrs.Attributes["QueueArn"]
	if sourceARN == "" {
		return errors.New("source queue ARN is missing")
	}
	allow, _ := json.Marshal(map[string]any{"redrivePermission": "byQueue", "sourceQueueArns": []string{sourceARN}})
	_, err = client.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{QueueUrl: dlq.QueueUrl, Attributes: map[string]string{"RedriveAllowPolicy": string(allow)}})
	return err
}
