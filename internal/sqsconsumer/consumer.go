package sqsconsumer

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

type Observer interface {
	Event(string, string)
	Duration(string, time.Duration)
	QueueDepth(string, float64)
}

type Consumer struct {
	observer         Observer
	client           *sqs.Client
	wagers           *application.WageringService
	logger           *slog.Logger
	queueURL, dlqURL string
	polling          context.Context
	cancel           context.CancelFunc
	done             chan struct{}
	healthy          atomic.Bool
}

func New(client *sqs.Client, wagers *application.WageringService, logger *slog.Logger) *Consumer {
	return &Consumer{client: client, wagers: wagers, logger: logger, done: make(chan struct{})}
}
func (c *Consumer) Start(ctx context.Context) error {
	started := false
	defer func() {
		if !started {
			c.closeClient()
		}
	}()
	queue, err := c.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(QueueName)})
	if err != nil {
		return errors.New("SQS source queue is unavailable")
	}
	dlq, err := c.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(DLQName)})
	if err != nil {
		return errors.New("SQS DLQ is unavailable")
	}
	c.queueURL, c.dlqURL = aws.ToString(queue.QueueUrl), aws.ToString(dlq.QueueUrl)
	if err := c.checkQueues(ctx); err != nil {
		return err
	}
	c.polling, c.cancel = context.WithCancel(context.Background())
	c.healthy.Store(true)
	started = true
	go c.run()
	c.logger.InfoContext(ctx, "SQS consumer started", "consumerName", ConsumerName)
	return nil
}
func (c *Consumer) StopPolling() {
	c.healthy.Store(false)
	if c.cancel != nil {
		c.cancel()
	}
}
func (c *Consumer) Wait(ctx context.Context) error {
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Consumer) Check(ctx context.Context) error {
	if c.polling == nil || c.polling.Err() != nil || !c.healthy.Load() {
		return errors.New("SQS consumer is unavailable")
	}
	if err := c.checkQueues(ctx); err != nil {
		return err
	}
	return observeDLQ(ctx, c.client, c.dlqURL, "input", c.observer)
}
func (c *Consumer) checkQueues(ctx context.Context) error {
	return checkFIFOQueues(ctx, c.client, c.queueURL, c.dlqURL)
}
func checkFIFOQueues(ctx context.Context, client *sqs.Client, queueURL, dlqURL string) error {
	source, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(queueURL), AttributeNames: []types.QueueAttributeName{
		types.QueueAttributeNameFifoQueue, types.QueueAttributeNameRedrivePolicy, types.QueueAttributeNameVisibilityTimeout,
	}})
	if err != nil {
		return errors.New("could not inspect SQS source queue")
	}
	dlq, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(dlqURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn, types.QueueAttributeNameFifoQueue}})
	if err != nil {
		return errors.New("could not inspect SQS DLQ")
	}
	var policy struct {
		DeadLetterTargetARN string      `json:"deadLetterTargetArn"`
		MaxReceiveCount     json.Number `json:"maxReceiveCount"`
	}
	if json.Unmarshal([]byte(source.Attributes["RedrivePolicy"]), &policy) != nil || policy.DeadLetterTargetARN == "" ||
		policy.DeadLetterTargetARN != dlq.Attributes["QueueArn"] || policy.MaxReceiveCount.String() != "5" ||
		source.Attributes["FifoQueue"] != "true" || dlq.Attributes["FifoQueue"] != "true" || source.Attributes["VisibilityTimeout"] != "60" {
		return errors.New("SQS FIFO, visibility or redrive configuration is invalid")
	}
	return nil
}
func (c *Consumer) run() {
	defer close(c.done)
	defer c.closeClient()
	defer c.healthy.Store(false)
	for c.polling.Err() == nil {
		output, err := c.client.ReceiveMessage(c.polling, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(c.queueURL), MaxNumberOfMessages: 1, WaitTimeSeconds: 10, VisibilityTimeout: VisibilitySeconds,
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount, types.MessageSystemAttributeNameMessageGroupId},
		})
		if err != nil {
			if c.polling.Err() != nil {
				return
			}
			c.healthy.Store(false)
			c.logger.Warn("SQS polling failed; retrying", "consumerName", ConsumerName)
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-c.polling.Done():
				timer.Stop()
				return
			case <-timer.C:
				continue
			}
		}
		c.healthy.Store(true)
		for _, message := range output.Messages {
			if c.polling.Err() != nil {
				c.visibility(message, 0)
				return
			}
			c.handle(message)
		}
	}
}
func (c *Consumer) handle(message types.Message) {
	started := time.Now()
	defer func() {
		if c.observer != nil {
			c.observer.Duration("sqs", time.Since(started))
		}
	}()
	c.event("received")
	// O cancelamento de long polling não interrompe o commit em andamento.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	event, input, err := decode(aws.ToString(message.Body))
	if err == nil && message.Attributes["MessageGroupId"] != MessageGroupID(input.ProviderID, input.WalletID) {
		err = application.ErrInvalidInput
	}
	var result application.ProcessWagerResult
	if err == nil {
		var actor application.Actor
		// The queue only accepts trusted internal publishers (IAM + queue
		// policy). External providers authenticate through HTTP/Keycloak and
		// never receive write credentials for this shared queue. Group IDs and
		// providerId in the body are routing/business data, not authentication.
		actor, err = application.NewProviderActor(input.ProviderID)
		if err == nil {
			result, err = c.wagers.ProcessMessage(ctx, actor, application.IncomingMessage{
				ConsumerName: ConsumerName, MessageID: event.MessageID, OccurredAt: event.OccurredAt,
			}, input)
		}
	}
	if err != nil {
		permanent := isPermanent(err)
		attempts, _ := strconv.Atoi(message.Attributes["ApproximateReceiveCount"])
		if attempts < 1 {
			attempts = 1
		}
		if attempts > 5 {
			attempts = 5
		}
		delay := int32(1 << attempts)
		if delay > 30 {
			delay = 30
		}
		if permanent {
			delay = 1
		}
		// Não apagar nem copiar manualmente: o redrive do broker leva a mensagem à DLQ.
		c.visibility(message, delay)
		c.event("retry")
		if attempts >= MaxReceives {
			c.event("redrive_expected")
		}
		c.logger.Warn("SQS message retained for retry or DLQ", "consumerName", ConsumerName,
			"messageId", event.MessageID, "brokerMessageId", aws.ToString(message.MessageId), "correlationId", input.Metadata.CorrelationID,
			"providerId", input.ProviderID, "walletId", input.WalletID, "attempt", attempts, "permanent", permanent)
		return
	}
	ack, cancelAck := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAck()
	_, err = c.client.DeleteMessage(ack, &sqs.DeleteMessageInput{QueueUrl: aws.String(c.queueURL), ReceiptHandle: message.ReceiptHandle})
	if err != nil {
		c.event("ack_error")
		c.logger.Warn("SQS acknowledgement failed after commit; redelivery is safe", "consumerName", ConsumerName,
			"messageId", event.MessageID, "brokerMessageId", aws.ToString(message.MessageId),
			"correlationId", input.Metadata.CorrelationID, "causationId", input.Metadata.CausationID,
			"transactionId", result.Transaction.ID, "providerId", input.ProviderID, "walletId", input.WalletID)
		return
	}
	c.event("completed")
	c.logger.Info("SQS message completed", "consumerName", ConsumerName, "messageId", event.MessageID,
		"brokerMessageId", aws.ToString(message.MessageId), "correlationId", input.Metadata.CorrelationID,
		"causationId", input.Metadata.CausationID, "transactionId", result.Transaction.ID, "providerId", input.ProviderID,
		"walletId", input.WalletID, "status", result.Transaction.Status, "idempotentReplay", result.IdempotentReplay)
}
func (c *Consumer) visibility(message types.Message, seconds int32) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(c.queueURL), ReceiptHandle: message.ReceiptHandle, VisibilityTimeout: seconds}); err != nil {
		c.logger.Warn("SQS visibility change failed; broker timeout will allow redelivery", "brokerMessageId", aws.ToString(message.MessageId))
	}
}
func isPermanent(err error) bool {
	for _, target := range []error{application.ErrInvalidInput, application.ErrInboxConflict, application.ErrForbidden, application.ErrUnauthenticated,
		application.ErrNotFound, domain.ErrInvalidMoney, domain.ErrInvalidAmount, domain.ErrInvalidCurrency,
		domain.ErrInvalidTransaction, domain.ErrInvalidTransactionAmount, domain.ErrInvalidReference} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func (c *Consumer) closeClient() {
	if client, ok := c.client.Options().HTTPClient.(interface{ CloseIdleConnections() }); ok {
		client.CloseIdleConnections()
	}
}

func (c *Consumer) SetObserver(observer Observer) { c.observer = observer }
func (c *Consumer) event(outcome string) {
	if c.observer != nil {
		c.observer.Event("sqs", outcome)
	}
}
func observeDLQ(ctx context.Context, client *sqs.Client, queueURL, name string, observer Observer) error {
	if observer == nil {
		return nil
	}
	result, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(queueURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages}})
	if err != nil {
		return errors.New("could not inspect SQS DLQ depth")
	}
	depth, err := strconv.ParseFloat(result.Attributes["ApproximateNumberOfMessages"], 64)
	if err != nil {
		return errors.New("invalid SQS DLQ depth")
	}
	observer.QueueDepth(name, depth)
	return nil
}
