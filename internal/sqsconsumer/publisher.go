package sqsconsumer

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
	"github.com/junglegaming/backend-challenge-go/internal/workers"
)

// EventPublisher envia o snapshot original, inclusive nas tentativas para DLQ.
type EventPublisher struct {
	client           *sqs.Client
	observer         Observer
	queueURL, dlqURL string
}

func NewEventPublisher(ctx context.Context, client *sqs.Client) (*EventPublisher, error) {
	publisher := &EventPublisher{client: client}
	queue, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(EventQueueName)})
	if err != nil {
		return nil, errors.New("SQS event queue is unavailable")
	}
	dlq, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(EventDLQName)})
	if err != nil {
		return nil, errors.New("SQS event DLQ is unavailable")
	}
	publisher.queueURL, publisher.dlqURL = aws.ToString(queue.QueueUrl), aws.ToString(dlq.QueueUrl)
	if err := publisher.Check(ctx); err != nil {
		return nil, err
	}
	return publisher, nil
}
func (p *EventPublisher) Check(ctx context.Context) error {
	if err := checkFIFOQueues(ctx, p.client, p.queueURL, p.dlqURL); err != nil {
		return err
	}
	return observeDLQ(ctx, p.client, p.dlqURL, "events", p.observer)
}
func (p *EventPublisher) Send(ctx context.Context, job workers.OutboxJob) error {
	var envelope struct {
		EventID   string `json:"eventId"`
		EventType string `json:"eventType"`
		Data      struct {
			WalletID string `json:"walletId"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(job.Payload), &envelope) != nil || envelope.EventID != job.EventID || envelope.Data.WalletID == "" {
		return workers.ErrPermanentDelivery
	}
	queue := p.queueURL
	if job.ToDLQ {
		queue = p.dlqURL
	}
	output, err := p.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: aws.String(queue), MessageBody: aws.String(job.Payload),
		MessageGroupId:         aws.String(DeduplicationID("wallet:" + envelope.Data.WalletID)),
		MessageDeduplicationId: aws.String(DeduplicationID(job.EventID)),
		MessageAttributes:      map[string]types.MessageAttributeValue{"eventType": {DataType: aws.String("String"), StringValue: aws.String(envelope.EventType)}},
	})
	if err != nil {
		var apiError smithy.APIError
		if errors.As(err, &apiError) {
			switch apiError.ErrorCode() {
			case "InvalidMessageContents", "InvalidParameterValue", "InvalidParameterCombination", "AccessDenied", "AccessDeniedException", "AWS.SimpleQueueService.NonExistentQueue":
				return workers.ErrPermanentDelivery
			}
		}
		return err
	}
	if output == nil || aws.ToString(output.MessageId) == "" {
		return errors.New("SQS did not confirm event delivery")
	}
	return nil
}
func (p *EventPublisher) Close() {
	if client, ok := p.client.Options().HTTPClient.(interface{ CloseIdleConnections() }); ok {
		client.CloseIdleConnections()
	}
}

func (p *EventPublisher) SetObserver(observer Observer) { p.observer = observer }
