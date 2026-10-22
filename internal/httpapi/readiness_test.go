package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestReadinessChecksRunConcurrently(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	check := func(ctx context.Context) error {
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	api := &api{config: Config{CheckPostgres: check, CheckSQS: check, Checks: map[string]func(context.Context) error{"idp": check, "workers": check}}}
	recorder := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { defer close(finished); api.ready(recorder, httptest.NewRequest("GET", "/health/ready", nil)) }()
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	close(release)
	<-finished
	if recorder.Code != 200 {
		t.Fatal(recorder.Body.String())
	}
}
func TestReadinessFailureDoesNotHideHealthyDependencies(t *testing.T) {
	good := func(context.Context) error { return nil }
	bad := func(context.Context) error { return errors.New("dependency unavailable") }
	for _, failure := range []string{"postgresql", "sqs", "idp", "workers", "schema"} {
		t.Run(failure, func(t *testing.T) {
			configuration := Config{CheckPostgres: good, CheckSQS: good, Checks: map[string]func(context.Context) error{"idp": good, "workers": good, "schema": good}}
			switch failure {
			case "postgresql":
				configuration.CheckPostgres = bad
			case "sqs":
				configuration.CheckSQS = bad
			default:
				configuration.Checks[failure] = bad
			}
			api := &api{config: configuration}
			recorder := httptest.NewRecorder()
			api.ready(recorder, httptest.NewRequest("GET", "/health/ready", nil))
			if recorder.Code != 503 {
				t.Fatal(recorder.Body.String())
			}
		})
	}
}
