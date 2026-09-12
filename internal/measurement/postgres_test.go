package measurement

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPostgresErrorRedactionRemovesConnectionCredentials(t *testing.T) {
	connection := "postgres://layercache:secret%20word@db.example.test/layercache?sslmode=require"
	err := redactPostgresError("connect", connection, errors.New(
		"dial "+connection+": password secret word and encoded secret%20word were rejected",
	))
	message := err.Error()
	for _, secret := range []string{connection, "secret word", "secret%20word"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error contains PostgreSQL credential %q: %s", secret, message)
		}
	}
}

func TestPostgresStoredOutcomeBoundsMetadata(t *testing.T) {
	base := time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC)
	outcome := FinalOutcome{
		RunID: "run-1", WorkID: strings.Repeat("w", maximumIdentityBytes+1),
		Result: ResultMiss, Source: SourceNone, StartedAt: base, FinishedAt: base.Add(time.Second),
	}
	if err := validateStoredOutcome(outcome); err == nil || !strings.Contains(err.Error(), "work ID") {
		t.Fatalf("unbounded work ID error = %v", err)
	}

	outcome.WorkID = "work-1"
	outcome.Dependencies = make([]string, maximumDependencies+1)
	for index := range outcome.Dependencies {
		outcome.Dependencies[index] = "dependency"
	}
	if err := validateStoredOutcome(outcome); err == nil || !strings.Contains(err.Error(), "dependencies") {
		t.Fatalf("unbounded dependencies error = %v", err)
	}
}

func TestPostgresStoredActionsMissCompletionBoundsMetadata(t *testing.T) {
	base := time.Date(2026, time.August, 31, 1, 0, 0, 0, time.UTC)
	completion := ActionsMissCompletion{
		RunID: "run-1", WorkID: "work-1", FinishedAt: base,
		ExecutionDuration: time.Second, UploadDuration: time.Second, UploadedBytes: 1024,
	}
	if err := validateStoredActionsMissCompletion(completion); err != nil {
		t.Fatalf("valid Actions miss completion: %v", err)
	}

	completion.RunID = strings.Repeat("r", maximumRunIDBytes+1)
	if err := validateStoredActionsMissCompletion(completion); err == nil || !strings.Contains(err.Error(), "run ID") {
		t.Fatalf("unbounded completion run ID error = %v", err)
	}
	completion.RunID = "run-1"
	completion.WorkID = strings.Repeat("w", maximumIdentityBytes+1)
	if err := validateStoredActionsMissCompletion(completion); err == nil || !strings.Contains(err.Error(), "work ID") {
		t.Fatalf("unbounded completion work ID error = %v", err)
	}
}
