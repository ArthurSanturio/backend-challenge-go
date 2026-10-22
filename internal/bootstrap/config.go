package bootstrap

import (
	"errors"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/httpapi"
)

// A configuração pertence à composição, não ao domínio ou aos casos de uso.
type config struct {
	databaseURL                             string
	httpAddress                             string
	oidc                                    httpapi.OIDCConfig
	sqsEnabled                              bool
	awsRegion                               string
	sqsEndpoint                             string
	workersEnabled                          bool
	outboxMaxAttempts, referenceMaxAttempts int
	referenceTTL                            time.Duration
}

func readConfig() (config, error) {
	port := os.Getenv("HTTP_PORT")
	if port == "" {
		port = "8080"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return config{}, errors.New("HTTP_PORT must be between 1 and 65535")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return config{}, errors.New("DATABASE_URL is required")
	}
	sqsEnabled := true
	if raw := os.Getenv("SQS_ENABLED"); raw != "" {
		sqsEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			return config{}, errors.New("SQS_ENABLED must be a boolean")
		}
	}
	workersEnabled := true
	if raw := os.Getenv("WORKERS_ENABLED"); raw != "" {
		workersEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			return config{}, errors.New("WORKERS_ENABLED must be a boolean")
		}
	}
	outboxAttempts, err := positiveEnv("OUTBOX_MAX_ATTEMPTS", 8)
	if err != nil {
		return config{}, err
	}
	referenceAttempts, err := positiveEnv("REFERENCE_MAX_ATTEMPTS", 100)
	if err != nil {
		return config{}, err
	}
	ttl := 24 * time.Hour
	if raw := os.Getenv("REFERENCE_TTL"); raw != "" {
		ttl, err = time.ParseDuration(raw)
		if err != nil || ttl <= 0 {
			return config{}, errors.New("REFERENCE_TTL must be a positive duration")
		}
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	return config{
		workersEnabled: workersEnabled, outboxMaxAttempts: outboxAttempts, referenceMaxAttempts: referenceAttempts, referenceTTL: ttl,
		sqsEnabled: sqsEnabled, awsRegion: region, sqsEndpoint: os.Getenv("SQS_ENDPOINT_URL"),
		databaseURL: databaseURL, httpAddress: net.JoinHostPort("", strconv.Itoa(number)),
		oidc: httpapi.OIDCConfig{Issuer: os.Getenv("OIDC_ISSUER_URL"), Audience: os.Getenv("OIDC_AUDIENCE"), JWKSURL: os.Getenv("OIDC_JWKS_URL")},
	}, nil
}

func positiveEnv(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 100000 {
		return 0, errors.New(name + " must be between 1 and 100000")
	}
	return value, nil
}
