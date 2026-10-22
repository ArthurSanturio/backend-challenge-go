package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/brokerauth"
	"github.com/junglegaming/backend-challenge-go/internal/sqsconsumer"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	client, err := sqsconsumer.NewClient(ctx, region, os.Getenv("SQS_ENDPOINT_URL"))
	if err == nil {
		err = sqsconsumer.Provision(ctx, client)
	}
	if err == nil {
		if os.Getenv("SQS_LOCAL_IAM_SETUP") == "true" {
			directory := os.Getenv("SQS_CREDENTIALS_DIRECTORY")
			if directory == "" {
				directory = "/credentials"
			}
			_, err = brokerauth.SetupLocal(ctx, client, directory)
			// The one-shot setup owns writable volumes; only the runtime's volume
			// is mounted (read-only) in the unprivileged API container.
			if err == nil && os.Geteuid() == 0 {
				for _, role := range []string{"runtime", "publisher", "events"} {
					for _, name := range []string{"", "credentials", "identity.json"} {
						if err = os.Chown(filepath.Join(directory, role, name), 65532, 65532); err != nil {
							break
						}
					}
					if err != nil {
						break
					}
				}
			}
		} else {
			err = brokerauth.Apply(ctx, client, brokerauth.Principals{
				Administrator:    os.Getenv("SQS_ADMIN_PRINCIPAL_ARN"),
				Runtime:          os.Getenv("SQS_RUNTIME_PRINCIPAL_ARN"),
				TrustedPublisher: os.Getenv("SQS_TRUSTED_PUBLISHER_PRINCIPAL_ARN"),
				EventConsumer:    os.Getenv("SQS_EVENT_CONSUMER_PRINCIPAL_ARN"),
			})
		}
	}
	if err != nil {
		logger.Error("SQS provisioning failed; check endpoint, credentials and permissions")
		os.Exit(1)
	}
	logger.Info("SQS queues and access policies provisioned; only trusted internal publishers may send requests")
}
