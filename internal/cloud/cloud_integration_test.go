package cloud

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/publictrust"
)

// TestConcurrentCloudSchemaMigrationAgainstPostgres is intentionally opt-in.
// Every migrator uses a separate pool and starts against one empty schema, as
// independent Layer Cache replicas do on their first deployment.
func TestConcurrentCloudSchemaMigrationAgainstPostgres(t *testing.T) {
	postgresURL := os.Getenv("LAYER_CACHE_QA_POSTGRES_URL")
	if postgresURL == "" {
		t.Skip("set LAYER_CACHE_QA_POSTGRES_URL for concurrent cloud migration QA")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	schema := fmt.Sprintf("layercache_qa_migration_%d", time.Now().UTC().UnixNano())
	admin, err := sql.Open("pgx", postgresURL)
	if err != nil {
		t.Fatalf("open PostgreSQL migration QA connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+quotePostgresIdentifier(schema)); err != nil {
		t.Fatalf("create isolated migration QA schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupContext, `DROP SCHEMA `+quotePostgresIdentifier(schema)+` CASCADE`); err != nil {
			t.Errorf("drop isolated migration QA schema: %v", err)
		}
	})

	scopedURL, err := postgresURLWithSearchPath(postgresURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	const replicas = 16
	databases := make([]*sql.DB, 0, replicas)
	t.Cleanup(func() {
		for _, database := range databases {
			_ = database.Close()
		}
	})
	for replica := 0; replica < replicas; replica++ {
		database, err := sql.Open("pgx", scopedURL)
		if err != nil {
			t.Fatal(err)
		}
		database.SetMaxOpenConns(1)
		if err := database.PingContext(ctx); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
		databases = append(databases, database)
	}

	ready := make(chan struct{}, replicas)
	start := make(chan struct{})
	errorsByReplica := make(chan error, replicas)
	var wait sync.WaitGroup
	for _, database := range databases {
		wait.Add(1)
		go func(database *sql.DB) {
			defer wait.Done()
			ready <- struct{}{}
			<-start
			store := &Store{database: database, config: Config{PostgresURL: scopedURL}}
			errorsByReplica <- store.migrate(ctx)
		}(database)
	}
	for replica := 0; replica < replicas; replica++ {
		<-ready
	}
	close(start)
	wait.Wait()
	close(errorsByReplica)
	for migrationErr := range errorsByReplica {
		if migrationErr != nil {
			t.Fatalf("concurrent cloud schema migration: %v", migrationErr)
		}
	}

	verification, err := sql.Open("pgx", scopedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer verification.Close()
	var currentSchema string
	if err := verification.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&currentSchema); err != nil {
		t.Fatal(err)
	}
	if currentSchema != schema {
		t.Fatalf("migration search path selected schema %q, want %q", currentSchema, schema)
	}
	var finalTable, auditTrigger bool
	if err := verification.QueryRowContext(ctx, `SELECT
		to_regclass('layercache_actions_entries_v1') IS NOT NULL,
		EXISTS (
			SELECT 1 FROM pg_trigger
			WHERE tgname = 'layercache_audit_append_only_v1'
				AND tgrelid = 'layercache_audit_events_v1'::regclass
		)`).Scan(&finalTable, &auditTrigger); err != nil {
		t.Fatal(err)
	}
	if !finalTable || !auditTrigger {
		t.Fatalf("concurrent migration result: final table=%t audit trigger=%t", finalTable, auditTrigger)
	}
}

// TestConfiguredMembershipReconciliationAgainstPostgres is intentionally
// opt-in. It verifies non-destructive Public startup and authoritative Team
// startup against the real PostgreSQL transaction and audit implementation.
func TestConfiguredMembershipReconciliationAgainstPostgres(t *testing.T) {
	postgresURL := os.Getenv("LAYER_CACHE_QA_POSTGRES_URL")
	if postgresURL == "" {
		t.Skip("set LAYER_CACHE_QA_POSTGRES_URL for cloud membership reconciliation QA")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	schema := fmt.Sprintf("layercache_qa_members_%d", time.Now().UTC().UnixNano())
	admin, err := sql.Open("pgx", postgresURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+quotePostgresIdentifier(schema)); err != nil {
		t.Fatalf("create membership QA schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupContext, `DROP SCHEMA `+quotePostgresIdentifier(schema)+` CASCADE`); err != nil {
			t.Errorf("drop membership QA schema: %v", err)
		}
	})
	scopedURL, err := postgresURLWithSearchPath(postgresURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("pgx", scopedURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := &Store{database: database, config: Config{
		PostgresURL: scopedURL, Project: "github.com/acme/widgets", MaxBytes: 1 << 30,
		Now: time.Now,
	}}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ensureProject(ctx, store); err != nil {
		t.Fatal(err)
	}
	for _, member := range []Member{
		{Subject: "github:alice", Role: "admin"},
		{Subject: "github:bob", Role: "writer"},
	} {
		if _, err := store.SetMember(ctx, "github:bootstrap", member); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.UpsertMembers(ctx, "public-configuration-bootstrap", []Member{
		{Subject: "github:alice", Role: "reader"},
		{Subject: "github:carol", Role: "admin"},
	}); err != nil {
		t.Fatalf("upsert Public server memberships: %v", err)
	}
	if role, err := store.MemberRole(ctx, store.config.Project, "github:alice"); err != nil || role != "reader" {
		t.Fatalf("upserted membership role = %q, %v", role, err)
	}
	if role, err := store.MemberRole(ctx, store.config.Project, "github:bob"); err != nil || role != "writer" {
		t.Fatalf("membership omitted by Public configuration = %q, %v", role, err)
	}
	if role, err := store.MemberRole(ctx, store.config.Project, "github:carol"); err != nil || role != "admin" {
		t.Fatalf("new Public membership role = %q, %v", role, err)
	}

	if err := store.ReconcileMembers(ctx, "configuration-bootstrap", []Member{
		{Subject: "github:alice", Role: "reader"},
	}); err != nil {
		t.Fatalf("reconcile configured memberships: %v", err)
	}
	if role, err := store.MemberRole(ctx, store.config.Project, "github:alice"); err != nil || role != "reader" {
		t.Fatalf("downgraded membership role = %q, %v", role, err)
	}
	if _, err := store.MemberRole(ctx, store.config.Project, "github:bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed membership lookup error = %v, want ErrNotFound", err)
	}
	if _, err := store.MemberRole(ctx, store.config.Project, "github:carol"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed Public-seeded membership lookup error = %v, want ErrNotFound", err)
	}
}

func postgresURLWithSearchPath(rawURL, schema string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse PostgreSQL migration QA URL: %w", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func quotePostgresIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// TestCloudStoreAgainstPostgresAndS3 is intentionally opt-in. qa/README.md
// starts disposable Postgres and MinIO containers and supplies these values.
func TestCloudStoreAgainstPostgresAndS3(t *testing.T) {
	postgresURL := os.Getenv("LAYER_CACHE_QA_POSTGRES_URL")
	s3Endpoint := os.Getenv("LAYER_CACHE_QA_S3_ENDPOINT")
	if postgresURL == "" || s3Endpoint == "" {
		t.Skip("set LAYER_CACHE_QA_POSTGRES_URL and LAYER_CACHE_QA_S3_ENDPOINT for cloud QA")
	}
	now := time.Now().UTC()
	project := "cloud-qa-" + strings.ToLower(time.Now().Format("20060102t150405.000000000"))
	config := Config{
		PostgresURL: postgresURL,
		S3Endpoint:  s3Endpoint,
		S3Bucket:    envOr("LAYER_CACHE_QA_S3_BUCKET", "layercache-qa"),
		S3Region:    envOr("LAYER_CACHE_QA_S3_REGION", "us-east-1"),
		// Empty explicit credentials exercise the normal AWS environment/file/IAM
		// credential chain. The QA command supplies AWS_ACCESS_KEY_ID and
		// AWS_SECRET_ACCESS_KEY.
		S3PathStyle: true,
		Project:     project,
		MaxBytes:    16,
		StageTTL:    time.Minute,
		BlobGrace:   time.Minute,
		Now:         func() time.Time { return now },
	}
	store, err := Open(context.Background(), config)
	if err != nil {
		t.Fatalf("open production cloud store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	firstKey := artifact.Key{
		Integration: "turbo", Project: project, Compatibility: "linux-amd64-glibc-schema1", Native: "first",
	}
	firstBody := []byte("winner1")
	first, created, err := store.Put(ctx, firstKey, artifact.Metadata{DurationMS: 10}, bytes.NewReader(firstBody))
	if err != nil || !created {
		t.Fatalf("publish first cloud artifact: created=%v err=%v", created, err)
	}
	if _, created, err := store.Put(ctx, firstKey, artifact.Metadata{DurationMS: 999}, bytes.NewReader(firstBody)); err != nil || created {
		t.Fatalf("idempotent cloud retry: created=%v err=%v", created, err)
	}
	if _, _, err := store.Put(ctx, firstKey, artifact.Metadata{}, bytes.NewReader([]byte("loserxx"))); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting cloud writer error = %v, want ErrConflict", err)
	}
	entry, body, err := store.Get(ctx, firstKey)
	if err != nil {
		t.Fatalf("open verified cloud artifact: %v", err)
	}
	contents, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(contents, firstBody) || entry.Digest != first.Digest {
		t.Fatalf("verified cloud read = %q, %v, %v, digest %q", contents, readErr, closeErr, entry.Digest)
	}

	secondKey := firstKey
	secondKey.Native = "second"
	secondBody := []byte("0123456789")
	second, created, err := store.Put(ctx, secondKey, artifact.Metadata{}, bytes.NewReader(secondBody))
	if err != nil || !created {
		t.Fatalf("publish quota-evicting artifact: created=%v err=%v", created, err)
	}
	if _, err := store.Head(ctx, firstKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LRU victim lookup error = %v, want ErrNotFound", err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.UsageBytes != int64(len(secondBody)) || stats.Entries != 1 {
		t.Fatalf("cloud quota statistics = %#v, %v", stats, err)
	}

	wrongStage, err := store.blobs.Stage(ctx, "projects/"+store.namespace+"/staging/corrupt", bytes.NewReader([]byte("abcdefghij")), 16, "application/octet-stream")
	if err != nil {
		t.Fatalf("stage corrupt-object fixture: %v", err)
	}
	if err := store.blobs.Commit(ctx, wrongStage, store.blobKey(second.Digest)); err != nil {
		t.Fatalf("replace object with corrupt fixture: %v", err)
	}
	if _, _, err := store.Get(ctx, secondKey); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt cloud read error = %v, want ErrCorrupt", err)
	}
	repairStage, err := store.blobs.Stage(ctx, "projects/"+store.namespace+"/staging/repair", bytes.NewReader(secondBody), 16, "application/octet-stream")
	if err != nil {
		t.Fatalf("stage object repair: %v", err)
	}
	if err := store.blobs.Commit(ctx, repairStage, store.blobKey(second.Digest)); err != nil {
		t.Fatalf("repair immutable object fixture: %v", err)
	}
	_ = store.blobs.Delete(ctx, wrongStage.Key)
	_ = store.blobs.Delete(ctx, repairStage.Key)

	if _, err := store.AppendAudit(ctx, AuditEvent{
		Actor: "github:qa", Action: "auth.denied", Resource: "route:/v8/artifacts", Outcome: "denied",
		Attributes: map[string]string{"reason": "wrong-project"},
	}); err != nil {
		t.Fatalf("append cloud audit event: %v", err)
	}
	events, err := store.ReadAudit(ctx, AuditQuery{Limit: 10})
	if err != nil || !hasAuditAction(events, "auth.denied") {
		t.Fatalf("cloud audit events = %#v, %v", events, err)
	}
	if _, err := store.SetMember(ctx, "github:admin", Member{Subject: "github:reader", Role: "reader"}); err != nil {
		t.Fatalf("set cloud membership: %v", err)
	}
	if role, err := store.MemberRole(ctx, project, "github:reader"); err != nil || role != "reader" {
		t.Fatalf("cloud member role = %q, %v", role, err)
	}

	publications, err := NewPublicationRegistry(store, time.Hour)
	if err != nil {
		t.Fatalf("open cloud Public Cache registry: %v", err)
	}
	publication := publictrust.Publication{
		Integration: "turbo", Project: project, Compatibility: secondKey.Compatibility, NativeKey: secondKey.Native,
		Repository: "https://github.com/layercache/qa", Commit: strings.Repeat("a", 40),
		RecipeDigest: "sha256:" + strings.Repeat("b", 64), Target: "build", Platform: "linux/amd64",
		Inputs:    []publictrust.DeclaredInput{{Name: "compatibility", Value: secondKey.Compatibility}},
		Toolchain: "node-24", Builder: "layercache-public-builder-v1", Digest: second.Digest,
		BuilderImageDigest: "sha256:" + strings.Repeat("e", 64),
		Size:               second.Size, DurationMS: 25, BuildID: "public-build-qa", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := publications.Publish(ctx, publication); err != nil {
		t.Fatalf("publish cloud Public Cache metadata: %v", err)
	}
	if resolved, err := publications.Resolve(ctx, publication.Identity()); err != nil || resolved.Identity() != publication.Identity() {
		t.Fatalf("resolve cloud Public Cache publication = %#v, %v", resolved, err)
	}
	buildIdentity := publictrust.BuildIdentity{
		Repository: publication.Repository, Commit: publication.Commit, Integration: publication.Integration,
		Target: publication.Target, RecipeDigest: publication.RecipeDigest, Platform: publication.Platform,
		Inputs: publication.Inputs,
	}
	if found, err := publications.FindBuild(ctx, buildIdentity); err != nil || found.Identity() != publication.Identity() {
		t.Fatalf("find cloud Public Build with exact inputs = %#v, %v", found, err)
	}
	buildIdentity.Inputs = []publictrust.DeclaredInput{{Name: "compatibility", Value: "linux-arm64"}}
	if _, err := publications.FindBuild(ctx, buildIdentity); !errors.Is(err, publictrust.ErrNotFound) {
		t.Fatalf("find cloud Public Build with wrong inputs error = %v", err)
	}
	if err := publications.MarkAmbiguous(ctx, publication.CacheIdentity(), strings.Repeat("f", 64)); !errors.Is(err, publictrust.ErrAmbiguous) {
		t.Fatalf("divergent cloud Public Cache publication error = %v, want ErrAmbiguous", err)
	}
	if _, err := publications.Resolve(ctx, publication.CacheIdentity()); !errors.Is(err, publictrust.ErrAmbiguous) {
		t.Fatalf("ambiguous cloud Public Cache resolution error = %v", err)
	}
	revocable := publication
	revocable.NativeKey = "revocable"
	revocable.BuildID = "public-build-revocable"
	revocableKey := secondKey
	revocableKey.Native = revocable.NativeKey
	if _, created, err := store.Put(ctx, revocableKey, artifact.Metadata{}, bytes.NewReader(secondBody)); err != nil || !created {
		t.Fatalf("publish revocable cloud artifact: created=%v err=%v", created, err)
	}
	if err := publications.Publish(ctx, revocable); err != nil {
		t.Fatalf("publish revocable cloud Public Cache metadata: %v", err)
	}
	if err := publications.RevokeAs(ctx, "github:admin", revocable.Identity(), "manual QA revocation", now); err != nil {
		t.Fatalf("revoke cloud Public Cache publication: %v", err)
	}
	if _, err := publications.Resolve(ctx, revocable.CacheIdentity()); !errors.Is(err, publictrust.ErrRevoked) {
		t.Fatalf("revoked cloud Public Cache resolution error = %v", err)
	}
	if err := store.Delete(ctx, revocableKey); err != nil {
		t.Fatalf("release revoked cloud artifact: %v", err)
	}

	lease, err := store.AcquirePromotion(ctx, project, "registry.example/cache:main", "runner:one", 10*time.Second)
	if err != nil {
		t.Fatalf("acquire promotion lease: %v", err)
	}
	if _, err := store.AcquirePromotion(ctx, project, lease.Reference, "runner:two", 10*time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("contended promotion lease error = %v, want ErrLeaseHeld", err)
	}
	originalExpiry := lease.ExpiresAt
	lease, err = store.RenewPromotion(ctx, lease, 15*time.Second)
	if err != nil {
		t.Fatalf("renew promotion lease: %v", err)
	}
	if !lease.ExpiresAt.After(originalExpiry) {
		t.Fatalf("renewed promotion expiry = %s, want after %s", lease.ExpiresAt, originalExpiry)
	}
	forged := lease
	forged.Token = "promotion-forged"
	if _, err := store.RenewPromotion(ctx, forged, 10*time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("forged promotion renewal error = %v, want ErrLeaseLost", err)
	}
	forged = lease
	forged.Owner = "runner:other"
	if _, err := store.RenewPromotion(ctx, forged, 10*time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("cross-owner promotion renewal error = %v, want ErrLeaseLost", err)
	}
	forged = lease
	forged.Token = "promotion-forged"
	if err := store.ReleasePromotion(ctx, forged); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("forged promotion release error = %v, want ErrLeaseLost", err)
	}
	if err := store.ReleasePromotion(ctx, lease); err != nil {
		t.Fatalf("release promotion lease: %v", err)
	}
	if err := store.ReleasePromotion(ctx, lease); err != nil {
		t.Fatalf("repeat promotion release: %v", err)
	}
	expiring, err := store.AcquirePromotion(ctx, project, lease.Reference, "runner:three", 10*time.Second)
	if err != nil {
		t.Fatalf("acquire expiring promotion lease: %v", err)
	}
	now = expiring.ExpiresAt.Add(time.Second)
	if _, err := store.AcquirePromotion(ctx, project, lease.Reference, "runner:four", 10*time.Second); err != nil {
		t.Fatalf("replace expired promotion lease: %v", err)
	}
	abandonedKey := "projects/" + store.namespace + "/staging/abandoned-qa"
	abandoned, err := store.blobs.Stage(ctx, abandonedKey, bytes.NewReader([]byte("abandoned")), 16, "application/octet-stream")
	if err != nil {
		t.Fatalf("stage abandoned upload fixture: %v", err)
	}
	if _, err := store.database.ExecContext(ctx, `
		INSERT INTO layercache_uploads_v1(
			upload_id, project_id, object_key, state, digest, size_bytes, created_at, expires_at
		) VALUES($1, $2, $3, 'staged', $4, $5, $6, $7)`,
		"abandoned-qa", project, abandonedKey, abandoned.Digest, abandoned.Size,
		now.Add(-2*time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatalf("record abandoned upload fixture: %v", err)
	}

	if err := store.Delete(ctx, secondKey); err != nil {
		t.Fatalf("release cloud cache entry: %v", err)
	}
	now = now.Add(2 * time.Minute)
	maintenance, err := store.Maintain(ctx)
	if err != nil || maintenance.DeletedBlobs < 1 || maintenance.ExpiredUploads != 1 {
		t.Fatalf("cloud maintenance = %#v, %v", maintenance, err)
	}
	if _, err := store.blobs.Stat(ctx, abandonedKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired staged cloud object error = %v, want ErrNotFound", err)
	}

	exerciseCloudActions(t, ctx, config)

	isolated := config
	isolated.Project = project + "-other"
	isolated.MaxBytes = 64
	other, err := Open(ctx, isolated)
	if err != nil {
		t.Fatalf("open isolated cloud project: %v", err)
	}
	defer other.Close()
	otherKey := firstKey
	otherKey.Project = isolated.Project
	otherKey.Native = "same-visible-key"
	if _, created, err := other.Put(ctx, otherKey, artifact.Metadata{}, bytes.NewReader([]byte("project-two"))); err != nil || !created {
		t.Fatalf("publish isolated project artifact: created=%v err=%v", created, err)
	}
	if _, err := store.Head(ctx, artifact.Key{
		Integration: otherKey.Integration, Project: project, Compatibility: otherKey.Compatibility, Native: otherKey.Native,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project lookup error = %v, want isolated miss", err)
	}

	concurrentConfig := config
	concurrentConfig.Project = project + "-concurrent"
	concurrentConfig.MaxBytes = 1 << 20
	concurrent, err := Open(ctx, concurrentConfig)
	if err != nil {
		t.Fatalf("open concurrent cloud project: %v", err)
	}
	defer concurrent.Close()
	concurrentKey := artifact.Key{
		Integration: "turbo", Project: concurrentConfig.Project,
		Compatibility: firstKey.Compatibility, Native: "thirty-two-writers",
	}
	payloads := [][]byte{[]byte("writer-a"), []byte("writer-b")}
	var wait sync.WaitGroup
	results := make(chan error, 32)
	createdCount := make(chan bool, 32)
	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func(payload []byte) {
			defer wait.Done()
			_, created, err := concurrent.Put(ctx, concurrentKey, artifact.Metadata{}, bytes.NewReader(payload))
			createdCount <- created
			results <- err
		}(payloads[index%len(payloads)])
	}
	wait.Wait()
	close(results)
	close(createdCount)
	winners := 0
	for created := range createdCount {
		if created {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent cloud publication winners = %d, want 1", winners)
	}
	for err := range results {
		if err != nil && !errors.Is(err, ErrConflict) {
			t.Fatalf("concurrent cloud publication error = %v", err)
		}
	}
	_, concurrentBody, err := concurrent.Get(ctx, concurrentKey)
	if err != nil {
		t.Fatalf("read concurrent cloud winner: %v", err)
	}
	winnerBytes, err := io.ReadAll(concurrentBody)
	_ = concurrentBody.Close()
	if err != nil || !bytes.Equal(winnerBytes, payloads[0]) && !bytes.Equal(winnerBytes, payloads[1]) {
		t.Fatalf("concurrent cloud winner = %q, %v", winnerBytes, err)
	}
}

func hasAuditAction(events []AuditEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}

func exerciseCloudActions(t *testing.T, ctx context.Context, base Config) {
	t.Helper()
	base.Project += "-actions"
	base.MaxBytes = 1 << 20
	store, err := Open(ctx, base)
	if err != nil {
		t.Fatalf("open cloud Actions store: %v", err)
	}
	defer store.Close()
	storage, err := NewActionsStorage(store)
	if err != nil {
		t.Fatalf("open cloud Actions index: %v", err)
	}
	scope := actionscache.Scope{
		Repository: "acme/widgets", Compatibility: "linux-amd64-node24",
		Ref: "refs/heads/feature", DefaultRef: "refs/heads/main",
	}
	payload := []byte("durable-cloud-actions")
	size := int64(len(payload))
	producerDuration := 42 * time.Second
	reservation, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "deps-linux-v2", Version: "archive-v1", CacheSize: &size,
	})
	if err != nil {
		t.Fatalf("reserve cloud Actions entry: %v", err)
	}
	middle := len(payload) / 2
	for _, chunk := range []struct {
		start int
		body  []byte
	}{{middle, payload[middle:]}, {0, payload[:middle]}} {
		if err := storage.Upload(ctx, actionscache.UploadRequest{
			ReservationID: reservation.ID, Scope: &scope, Start: int64(chunk.start),
			End: int64(chunk.start + len(chunk.body) - 1), Body: bytes.NewReader(chunk.body),
		}); err != nil {
			t.Fatalf("upload cloud Actions range at %d: %v", chunk.start, err)
		}
	}
	entry, err := storage.Commit(ctx, actionscache.CommitRequest{
		ReservationID: reservation.ID, Scope: &scope, Size: size, ProducerDuration: &producerDuration,
	})
	if err != nil {
		t.Fatalf("commit cloud Actions entry: %v", err)
	}
	lookup, err := storage.Lookup(ctx, actionscache.LookupRequest{
		Scope: scope, Keys: []string{"missing", "deps-linux-"}, Version: "archive-v1",
	})
	if err != nil || lookup.Match != actionscache.MatchPrefix || lookup.RefScope != actionscache.RefScopeCurrent {
		t.Fatalf("cloud Actions prefix lookup = %#v, %v", lookup, err)
	}

	reopened, err := Open(ctx, base)
	if err != nil {
		t.Fatalf("reopen cloud Actions store: %v", err)
	}
	defer reopened.Close()
	reopenedActions, err := NewActionsStorage(reopened)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopenedActions.Lookup(ctx, actionscache.LookupRequest{
		Scope: scope, Keys: []string{"deps-linux-v2"}, Version: "archive-v1",
	})
	if err != nil || persisted.Entry.ID != entry.ID || persisted.Match != actionscache.MatchExact ||
		persisted.Entry.ProducerDuration == nil || *persisted.Entry.ProducerDuration != producerDuration {
		t.Fatalf("reopened cloud Actions lookup = %#v, %v", persisted, err)
	}
	archive, err := reopenedActions.Open(ctx, actionscache.OpenRequest{Scope: scope, ID: persisted.Entry.ID})
	if err != nil {
		t.Fatalf("open persisted cloud Actions archive: %v", err)
	}
	got, readErr := io.ReadAll(archive.Body)
	closeErr := archive.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, payload) {
		t.Fatalf("persisted cloud Actions archive = %q, read %v, close %v", got, readErr, closeErr)
	}
	if _, err := reopenedActions.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "deps-linux-v2", Version: "archive-v1", CacheSize: &size,
	}); !errors.Is(err, actionscache.ErrAlreadyExists) {
		t.Fatalf("duplicate cloud Actions reservation error = %v, want ErrAlreadyExists", err)
	}
	reservationRecord := cloudActionsReservation{
		repository: scope.Repository, compatibility: scope.Compatibility,
		ref: scope.Ref, key: "deps-linux-v2", version: "archive-v1",
	}
	actionsKey := reopenedActions.artifactKey(reservationRecord)
	actionsArtifact, err := reopened.Head(ctx, actionsKey)
	if err != nil {
		t.Fatal(err)
	}
	corruptPayload := bytes.Repeat([]byte{'x'}, len(payload))
	corrupt, err := reopened.blobs.Stage(
		ctx, "projects/"+reopened.namespace+"/staging/actions-corrupt",
		bytes.NewReader(corruptPayload), int64(len(corruptPayload)), "application/octet-stream",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.blobs.Commit(ctx, corrupt, reopened.blobKey(actionsArtifact.Digest)); err != nil {
		t.Fatal(err)
	}
	if _, err := reopenedActions.Lookup(ctx, actionscache.LookupRequest{
		Scope: scope, Keys: []string{"deps-linux-v2"}, Version: "archive-v1",
	}); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("corrupt cloud Actions lookup error = %v, want safe miss", err)
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
