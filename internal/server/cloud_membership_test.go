package server

import (
	"context"
	"testing"

	"github.com/layercache/layercache/internal/cloud"
	"github.com/layercache/layercache/internal/config"
)

type recordingCloudMembershipStore struct {
	operation string
	actor     string
	members   []cloud.Member
}

func (store *recordingCloudMembershipStore) UpsertMembers(_ context.Context, actor string, members []cloud.Member) error {
	store.operation = "upsert"
	store.actor = actor
	store.members = append([]cloud.Member(nil), members...)
	return nil
}

func (store *recordingCloudMembershipStore) ReconcileMembers(_ context.Context, actor string, members []cloud.Member) error {
	store.operation = "reconcile"
	store.actor = actor
	store.members = append([]cloud.Member(nil), members...)
	return nil
}

func TestConfiguredCloudMembershipPolicyMatchesServerRole(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		role      string
		operation string
	}{
		{role: "team", operation: "reconcile"},
		{role: "public", operation: "upsert"},
	} {
		test := test
		t.Run(test.role, func(t *testing.T) {
			t.Parallel()
			memberships := &recordingCloudMembershipStore{}
			server := &Server{config: config.Config{
				Role: test.role, ProjectID: "github.com/acme/widgets",
				TeamMembers: map[string]string{"Alice": "ADMIN", "bob": "writer"},
			}}
			if err := server.applyConfiguredCloudMemberships(context.Background(), memberships); err != nil {
				t.Fatal(err)
			}
			if memberships.operation != test.operation {
				t.Fatalf("membership operation = %q, want %q", memberships.operation, test.operation)
			}
			if memberships.actor != "configuration-bootstrap" {
				t.Fatalf("membership actor = %q", memberships.actor)
			}
			got := make(map[string]string, len(memberships.members))
			for _, member := range memberships.members {
				if member.Project != server.config.ProjectID {
					t.Fatalf("membership project = %q", member.Project)
				}
				got[member.Subject] = member.Role
			}
			if got["github:alice"] != "admin" || got["github:bob"] != "writer" || len(got) != 2 {
				t.Fatalf("configured memberships = %#v", got)
			}
		})
	}
}
