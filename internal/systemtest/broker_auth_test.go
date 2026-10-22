//go:build integration

package systemtest

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
	"github.com/junglegaming/backend-challenge-go/internal/brokerauth"
	"github.com/junglegaming/backend-challenge-go/internal/sqsconsumer"
)

func brokerAs(admin *sqs.Client, credentials aws.Credentials) *sqs.Client {
	options := admin.Options()
	options.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return credentials, nil })
	return sqs.New(options)
}

func TestSQSBrokerAuthorization(t *testing.T) {
	endpoint := os.Getenv("SQS_TEST_ENDPOINT_URL")
	if endpoint == "" {
		t.Fatal("run scripts/test-system.sh to start an IAM-enabled broker")
	}
	admin, err := sqsconsumer.NewClient(t.Context(), "us-east-1", endpoint)
	must(t, err)
	must(t, sqsconsumer.Provision(t.Context(), admin))
	directory := t.TempDir()
	identities, err := brokerauth.SetupLocal(t.Context(), admin, directory)
	must(t, err)
	repeated, err := brokerauth.SetupLocal(t.Context(), admin, directory)
	must(t, err)
	if repeated.Runtime.Credentials.AccessKeyID != identities.Runtime.Credentials.AccessKeyID {
		t.Fatal("repeated setup rotated a running service's credentials")
	}
	for _, role := range []string{"runtime", "publisher", "events"} {
		file, err := os.Stat(filepath.Join(directory, role, "credentials"))
		must(t, err)
		if file.Mode().Perm() != 0600 {
			t.Fatal("credential file must only be readable by its owner")
		}
	}
	input, err := admin.GetQueueUrl(t.Context(), &sqs.GetQueueUrlInput{QueueName: aws.String(sqsconsumer.QueueName)})
	must(t, err)
	events, err := admin.GetQueueUrl(t.Context(), &sqs.GetQueueUrlInput{QueueName: aws.String(sqsconsumer.EventQueueName)})
	must(t, err)
	for _, queue := range []*string{input.QueueUrl, events.QueueUrl} {
		_, err := admin.PurgeQueue(t.Context(), &sqs.PurgeQueueInput{QueueUrl: queue})
		must(t, err)
		queue := queue
		t.Cleanup(func() { _, _ = admin.PurgeQueue(context.Background(), &sqs.PurgeQueueInput{QueueUrl: queue}) })
	}
	runtime := brokerAs(admin, identities.Runtime.Credentials)
	publisher := brokerAs(admin, identities.Publisher.Credentials)
	reader := brokerAs(admin, identities.Events.Credentials)
	// Even an accidental broad same-account IAM grant cannot override the
	// explicit Deny on the queue resource policy.
	iamAdmin, err := brokerauth.LocalIAM(admin)
	must(t, err)
	name := "external-provider-" + rand.Text()
	_, err = iamAdmin.CreateUser(t.Context(), &iam.CreateUserInput{UserName: &name})
	must(t, err)
	_, err = iamAdmin.PutUserPolicy(t.Context(), &iam.PutUserPolicyInput{UserName: &name, PolicyName: aws.String("accidental-broad-grant"), PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sqs:*","Resource":"*"}]}`)})
	must(t, err)
	key, err := iamAdmin.CreateAccessKey(t.Context(), &iam.CreateAccessKeyInput{UserName: &name})
	must(t, err)
	outsider := brokerAs(admin, aws.Credentials{AccessKeyID: aws.ToString(key.AccessKey.AccessKeyId), SecretAccessKey: aws.ToString(key.AccessKey.SecretAccessKey)})
	send := func(client *sqs.Client, queue *string) error {
		_, err := client.SendMessage(t.Context(), &sqs.SendMessageInput{QueueUrl: queue, MessageBody: aws.String(`{"providerId":"provider-a"}`), MessageGroupId: aws.String("authorization"), MessageDeduplicationId: aws.String(rand.Text())})
		return err
	}
	denied := func(t *testing.T, err error) {
		t.Helper()
		var api smithy.APIError
		if !errors.As(err, &api) || (api.ErrorCode() != "AccessDenied" && api.ErrorCode() != "AccessDeniedException") {
			t.Fatalf("want broker AccessDenied, got %v", err)
		}
	}
	authenticationDenied := func(t *testing.T, err error) {
		t.Helper()
		var api smithy.APIError
		if !errors.As(err, &api) {
			t.Fatalf("want broker authentication error, got %v", err)
		}
		switch api.ErrorCode() {
		case "SignatureDoesNotMatch", "InvalidSignatureException", "InvalidClientTokenId", "UnrecognizedClientException", "AccessDenied", "AccessDeniedException":
		default:
			t.Fatalf("unexpected authentication error code: %s", api.ErrorCode())
		}
	}
	for name, client := range map[string]*sqs.Client{"runtime": runtime, "event-consumer": reader, "external-provider-with-broad-IAM": outsider} {
		t.Run(name+"_cannot_publish_requests", func(t *testing.T) { denied(t, send(client, input.QueueUrl)) })
	}
	for name, client := range map[string]*sqs.Client{"trusted-publisher": publisher, "external-provider-with-broad-IAM": outsider} {
		t.Run(name+"_cannot_consume_or_administer", func(t *testing.T) {
			_, err := client.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{QueueUrl: input.QueueUrl, WaitTimeSeconds: 0})
			denied(t, err)
			_, err = client.SetQueueAttributes(t.Context(), &sqs.SetQueueAttributesInput{QueueUrl: input.QueueUrl, Attributes: map[string]string{"Policy": "{}"}})
			denied(t, err)
			_, err = client.PurgeQueue(t.Context(), &sqs.PurgeQueueInput{QueueUrl: input.QueueUrl})
			denied(t, err)
		})
	}
	t.Run("external_provider_cannot_bypass_using_batch", func(t *testing.T) {
		_, err := outsider.SendMessageBatch(t.Context(), &sqs.SendMessageBatchInput{QueueUrl: input.QueueUrl, Entries: []types.SendMessageBatchRequestEntry{{Id: aws.String("forged"), MessageBody: aws.String(`{"providerId":"provider-b"}`), MessageGroupId: aws.String("authorization"), MessageDeduplicationId: aws.String(rand.Text())}}})
		denied(t, err)
	})
	t.Run("invalid_signature_is_rejected", func(t *testing.T) {
		forged := identities.Publisher.Credentials
		forged.SecretAccessKey = "wrong-secret"
		authenticationDenied(t, send(brokerAs(admin, forged), input.QueueUrl))
	})
	t.Run("unknown_provider_credentials_are_rejected", func(t *testing.T) {
		authenticationDenied(t, send(brokerAs(admin, aws.Credentials{AccessKeyID: "AKIAEXTERNALPROVIDER0", SecretAccessKey: "provider-a-local-secret"}), input.QueueUrl))
	})
	t.Run("only_trusted_publisher_sends_and_runtime_consumes", func(t *testing.T) {
		must(t, send(publisher, input.QueueUrl))
		messages, err := runtime.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{QueueUrl: input.QueueUrl, WaitTimeSeconds: 1})
		must(t, err)
		if len(messages.Messages) != 1 {
			t.Fatal("expected exactly the authorized publication")
		}
		_, err = runtime.DeleteMessage(t.Context(), &sqs.DeleteMessageInput{QueueUrl: input.QueueUrl, ReceiptHandle: messages.Messages[0].ReceiptHandle})
		must(t, err)
	})
	t.Run("runtime_publishes_events_but_cannot_consume_them", func(t *testing.T) {
		denied(t, send(publisher, events.QueueUrl))
		must(t, send(runtime, events.QueueUrl))
		_, err := runtime.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{QueueUrl: events.QueueUrl, WaitTimeSeconds: 0})
		denied(t, err)
		messages, err := reader.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{QueueUrl: events.QueueUrl, WaitTimeSeconds: 1})
		must(t, err)
		if len(messages.Messages) != 1 {
			t.Fatal("authorized event consumer could not read the event")
		}
	})
}
