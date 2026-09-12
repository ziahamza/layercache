package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPublicBuildHumanResultsIncludeActionableState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		body    string
		want    []string
	}{
		{
			name:    "request",
			message: "Public Build requested",
			body:    `{"build":{"id":"build-1","state":"queued","request":{"repository":"https://github.com/acme/widget","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","integration":"turbo","target":"@acme/widget#compile","platform":"linux/amd64"},"requestedAt":"2026-08-31T10:00:00Z"},"reused":false}`,
			want: []string{
				"Public Build requested", "Reused: no", "Build: build-1", "State: queued",
				"Source: https://github.com/acme/widget@aaaaaaaa", "Request: turbo @acme/widget#compile on linux/amd64",
			},
		},
		{
			name:    "worker lease",
			message: "Public Build leased",
			body:    `{"leaseToken":"lease-secret","workerId":"worker-1","leasedAt":"2026-08-31T10:01:00Z","expiresAt":"2026-08-31T10:02:00Z","build":{"id":"build-1","state":"running","workerId":"worker-1"}}`,
			want: []string{
				"Public Build leased", "Worker: worker-1", "Lease expires: 2026-08-31T10:02:00Z",
				"Lease token: lease-secret", "Build: build-1", "State: running",
			},
		},
		{
			name:    "logs",
			message: "Public Build logs",
			body:    `{"buildId":"build-1","logs":[{"sequence":3,"timestamp":"2026-08-31T10:03:00Z","message":"restored verified output"}]}`,
			want: []string{
				"Public Build logs", "Build: build-1", "[2026-08-31T10:03:00Z] #3 restored verified output",
			},
		},
		{
			name:    "cancel",
			message: "Public Build cancelled",
			body:    `{"id":"build-2","state":"cancelled","finishedAt":"2026-08-31T10:04:00Z"}`,
			want:    []string{"Public Build cancelled", "Build: build-2", "State: cancelled", "Finished: 2026-08-31T10:04:00Z"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			if err := printPublicBuildResult(&output, false, []byte(test.body), test.message); err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("human output omitted %q:\n%s", want, output.String())
				}
			}
		})
	}
}

func TestPublicBuildJSONResultKeepsResponseShape(t *testing.T) {
	t.Parallel()

	input := []byte(`{"build":{"id":"build-1","state":"queued"},"reused":false}`)
	var output bytes.Buffer
	if err := printPublicBuildResult(&output, true, input, "ignored"); err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(input, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, output.String())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON result changed shape: got %#v, want %#v", got, want)
	}
}
