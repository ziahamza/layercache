package portal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/compatibility"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/server"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	postgresURL := os.Getenv("LAYERCACHE_PORTAL_TEST_POSTGRES_URL")
	if postgresURL != "" {
		endpoint, err := url.Parse(postgresURL)
		if err != nil || (endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql") || (endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost" && endpoint.Hostname() != "::1") || endpoint.Path != "/layercache_portal_test" {
			t.Fatal("PostgreSQL QA requires a loopback URL to the dedicated layercache_portal_test database")
		}
		admin, err := sql.Open("pgx", postgresURL)
		if err != nil {
			t.Fatal(err)
		}
		admin.SetMaxOpenConns(1)
		schema := randomID("qa_")
		if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			defer admin.Close()
			if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
				t.Error(err)
			}
		})
		query := endpoint.Query()
		query.Set("search_path", schema)
		endpoint.RawQuery = query.Encode()
		postgresURL = endpoint.String()
	}
	s, err := OpenStore(context.Background(), postgresURL, filepath.Join(t.TempDir(), "portal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, u := range []User{{"1", "owner"}, {"2", "member"}, {"3", "outsider"}} {
		if err := s.UpsertUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func mustTeam(t *testing.T, s *Store, user string) Team {
	t.Helper()
	v, err := s.CreateTeam(context.Background(), user, "Example team")
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func mustProject(t *testing.T, s *Store, user, team, repo string) Project {
	t.Helper()
	v, err := s.CreateProject(context.Background(), user, team, "Example project", repo, "123", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func mustJoin(t *testing.T, s *Store, owner, team, user, role string) {
	t.Helper()
	inv, err := s.Invite(context.Background(), owner, team, user, role)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Accept(context.Background(), user, inv.ID); err != nil {
		t.Fatal(err)
	}
}
func requireErr(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got error %v; want %v", got, want)
	}
}

func TestLegacyProjectMigrationFailsClosedUntilOriginalRepositoryIDRestored(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	filename := filepath.Join(dataDir, "portal.db")
	legacy, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE lc_portal_projects (id TEXT PRIMARY KEY, team_id TEXT NOT NULL, name TEXT NOT NULL, repository TEXT NOT NULL, default_ref TEXT NOT NULL, secret TEXT NOT NULL, UNIQUE(team_id,repository))`,
		`INSERT INTO lc_portal_projects(id,team_id,name,repository,default_ref,secret) VALUES('project-legacy','team-legacy','Legacy','acme/reused','refs/heads/main','secret-legacy')`,
	} {
		if _, err := legacy.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, "", filename)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := store.AllProjects(ctx)
	if err != nil || len(projects) != 1 || projects[0].RepositoryID != "" || projects[0].Repository != "acme/reused" {
		t.Fatalf("legacy row after migration: %+v, %v", projects, err)
	}
	if _, err := store.RepositoryID(ctx, "project-legacy"); !errors.Is(err, ErrDenied) {
		t.Fatalf("legacy blank ID lookup = %v, want denied", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	template, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	template.MinFreeBytes = 0
	cfg := Config{Origin: "https://cloud.example", DataDir: dataDir, SessionKey: []byte(strings.Repeat("k", 32)), GitHubClientID: "client", GitHubClientSecret: "secret", ProjectTemplate: template}
	if portal, err := New(ctx, cfg); err == nil {
		_ = portal.Close()
		t.Fatal("legacy project started without its original repository ID")
	} else if !strings.Contains(err.Error(), "restore the original numeric ID") {
		t.Fatalf("legacy recovery error: %v", err)
	}
	store, err = OpenStore(ctx, "", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE lc_portal_projects SET repository_id='123' WHERE id='project-legacy'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	portal, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("restart after restoring trusted original ID: %v", err)
	}
	defer portal.Close()
}

func TestStorePersistenceIdentityIsolation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	team := mustTeam(t, s, "1")
	project := mustProject(t, s, "1", team.ID, "example/repo")
	mustJoin(t, s, "1", team.ID, "2", "writer")
	if err := s.UpsertUser(ctx, User{"2", "renamed"}); err != nil {
		t.Fatal(err)
	}
	// A new account that acquires the old login must not acquire the old ID's role.
	if err := s.UpsertUser(ctx, User{"3", "member"}); err != nil {
		t.Fatal(err)
	}
	if role, err := s.MemberRole(ctx, project.ID, "github-id:2"); err != nil || role != "writer" {
		t.Fatalf("renamed identity: %q %v", role, err)
	}
	for _, subject := range []string{"github-id:3", "github:member", "github:renamed", "runtime-admin:2", "github-id:"} {
		_, err := s.MemberRole(ctx, project.ID, subject)
		requireErr(t, err, ErrDenied)
	}
	otherTeam := mustTeam(t, s, "3")
	otherProject := mustProject(t, s, "3", otherTeam.ID, "example/repo")
	if project.ID == otherProject.ID || project.Secret == otherProject.Secret {
		t.Fatal("projects share authority")
	}
	_, err := s.MemberRole(ctx, otherProject.ID, "github-id:2")
	requireErr(t, err, ErrDenied)
	projects, err := s.Projects(ctx, "2")
	if err != nil || len(projects) != 1 || projects[0].ID != project.ID {
		t.Fatalf("project isolation: %+v %v", projects, err)
	}
	encoded, err := json.Marshal(projects)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), project.Secret) || strings.Contains(string(encoded), "secret") {
		t.Fatal("JSON exposes project signing secret")
	}
	var filename string
	postgresURL := os.Getenv("LAYERCACHE_PORTAL_TEST_POSTGRES_URL")
	if postgresURL == "" {
		if err := s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&filename); err != nil {
			t.Fatal(err)
		}
	} else {
		var schema string
		if err := s.db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
			t.Fatal(err)
		}
		endpoint, _ := url.Parse(postgresURL)
		query := endpoint.Query()
		query.Set("search_path", schema)
		endpoint.RawQuery = query.Encode()
		postgresURL = endpoint.String()
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(ctx, postgresURL, filename)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.AllProjects(ctx)
	if err != nil || len(stored) != 2 {
		t.Fatalf("persisted projects: %+v %v", stored, err)
	}
	role, err := reopened.MemberRole(ctx, project.ID, "github-id:2")
	if err != nil || role != "writer" {
		t.Fatalf("persisted membership %q %v", role, err)
	}
	if postgresURL == "" {
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("database permissions %o", info.Mode().Perm())
		}
	}
	members, err := reopened.Members(ctx, "1", team.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.UserID == "2" && m.Login != "renamed" {
			t.Fatalf("stale display identity: %+v", m)
		}
	}
}

func TestStoreInvitationsAreTargetedSingleUseAndRevocable(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	team := mustTeam(t, s, "1")
	project := mustProject(t, s, "1", team.ID, "example/repo")
	first, err := s.Invite(ctx, "1", team.ID, "2", "writer")
	if err != nil {
		t.Fatal(err)
	}
	requireErr(t, s.Accept(ctx, "3", first.ID), ErrDenied)
	pending, err := s.Invitations(ctx, "3")
	if err != nil || len(pending) != 0 {
		t.Fatalf("outsider invitations: %+v %v", pending, err)
	}
	replacement, err := s.Invite(ctx, "1", team.ID, "2", "reader")
	if err != nil {
		t.Fatal(err)
	}
	requireErr(t, s.Accept(ctx, "2", first.ID), ErrDenied)
	if err := s.Accept(ctx, "2", replacement.ID); err != nil {
		t.Fatal(err)
	}
	requireErr(t, s.Accept(ctx, "2", replacement.ID), ErrDenied)
	role, err := s.MemberRole(ctx, project.ID, "github-id:2")
	if err != nil || role != "reader" {
		t.Fatalf("replacement role: %q %v", role, err)
	}
	_, err = s.Invite(ctx, "1", team.ID, "2", "admin")
	requireErr(t, err, ErrConflict)
	// Emulate an outstanding invitation from an older release, then remove membership.
	_, err = s.db.Exec(`INSERT INTO lc_portal_invites(id,team_id,user_id,role,expires_at) VALUES('legacy',$1,'2','admin',$2)`, team.ID, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeMember(ctx, "1", team.ID, "2", ""); err != nil {
		t.Fatal(err)
	}
	requireErr(t, s.Accept(ctx, "2", "legacy"), ErrDenied)
	_, err = s.MemberRole(ctx, project.ID, "github-id:2")
	requireErr(t, err, ErrDenied)
	projects, err := s.Projects(ctx, "2")
	if err != nil || len(projects) != 0 {
		t.Fatalf("removed member sees projects: %+v %v", projects, err)
	}
	_, err = s.Members(ctx, "2", team.ID)
	requireErr(t, err, ErrDenied)
	inv, err := s.Invite(ctx, "1", team.ID, "2", "writer")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE lc_portal_invites SET expires_at=$1 WHERE id=$2`, time.Now().Add(-time.Second).Unix(), inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	requireErr(t, s.Accept(ctx, "2", inv.ID), ErrDenied)
	pending, err = s.Invitations(ctx, "2")
	if err != nil || len(pending) != 0 {
		t.Fatalf("expired invitations: %+v %v", pending, err)
	}
}

func TestStoreAuthorizationAndLastAdministrator(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	team := mustTeam(t, s, "1")
	mustJoin(t, s, "1", team.ID, "2", "writer")
	for _, role := range []string{"reader", "writer", ""} {
		requireErr(t, s.ChangeMember(ctx, "1", team.ID, "1", role), ErrConflict)
	}
	requireErr(t, s.ChangeMember(ctx, "2", team.ID, "2", "admin"), ErrDenied)
	_, err := s.Invite(ctx, "2", team.ID, "3", "admin")
	requireErr(t, err, ErrDenied)
	_, err = s.CreateProject(ctx, "2", team.ID, "No", "example/no", "123", "refs/heads/main")
	requireErr(t, err, ErrDenied)
	_, err = s.CreateProject(ctx, "3", team.ID, "No", "example/no", "123", "refs/heads/main")
	requireErr(t, err, ErrDenied)
	requireErr(t, s.ChangeMember(ctx, "1", team.ID, "2", "owner"), ErrInvalid)
	if err := s.ChangeMember(ctx, "1", team.ID, "2", "admin"); err != nil {
		t.Fatal(err)
	}
	// Competing self-removals must preserve at least one administrator.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, user := range []string{"1", "2"} {
		wg.Add(1)
		go func(user string) { defer wg.Done(); results <- s.ChangeMember(ctx, user, team.ID, user, "") }(user)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("unexpected removal error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("successful removals = %d", success)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM lc_portal_members WHERE team_id=$1 AND role='admin'`, team.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("remaining admins = %d, %v", count, err)
	}
}

func TestStoreLimitsAndInvalidInput(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for _, name := range []string{"", " leading", "trailing ", "line\nbreak", strings.Repeat("x", 81)} {
		_, err := s.CreateTeam(ctx, "1", name)
		requireErr(t, err, ErrInvalid)
	}
	team := mustTeam(t, s, "1")
	p := mustProject(t, s, "1", team.ID, "example/one")
	_, err := s.CreateProject(ctx, "1", team.ID, "Duplicate", p.Repository, p.RepositoryID, p.DefaultRef)
	requireErr(t, err, ErrConflict)
	_, err = s.CreateProject(ctx, "1", team.ID, "Case duplicate", strings.ToUpper(p.Repository), p.RepositoryID, p.DefaultRef)
	requireErr(t, err, ErrConflict)
	for i := 1; i < 10; i++ {
		mustProject(t, s, "1", team.ID, fmt.Sprintf("example/repo%d", i))
	}
	_, err = s.CreateProject(ctx, "1", team.ID, "Overflow", "example/overflow", "123", "refs/heads/main")
	requireErr(t, err, ErrLimit)
	for i := 1; i < 5; i++ {
		mustTeam(t, s, "1")
	}
	_, err = s.CreateTeam(ctx, "1", "Overflow")
	requireErr(t, err, ErrLimit)
}

func TestStoreSessionsAndOAuth(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now()
	for i := 0; i < 11; i++ {
		err := s.SaveSession(ctx, fmt.Sprintf("token-%d", i), Session{User: User{ID: "1"}, CSRF: "csrf", EncryptedToken: "encrypted", ExpiresAt: now.Add(time.Duration(i+1) * time.Hour).Unix()})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Session(ctx, "token-0")
	requireErr(t, err, ErrDenied)
	session, err := s.Session(ctx, "token-10")
	if err != nil || session.User.ID != "1" || session.User.Login != "owner" || session.EncryptedToken != "encrypted" {
		t.Fatalf("session: %+v %v", session, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM lc_portal_sessions WHERE id LIKE 'token-%'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("raw session IDs stored: %d %v", count, err)
	}
	if err := s.DeleteSession(ctx, "token-10"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Session(ctx, "token-10")
	requireErr(t, err, ErrDenied)
	if err := s.SaveSession(ctx, "expired", Session{User: User{ID: "2"}, CSRF: "csrf", ExpiresAt: now.Add(-time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Session(ctx, "expired")
	requireErr(t, err, ErrDenied)
	if err := s.SaveOAuth(ctx, "state", "verifier"); err != nil {
		t.Fatal(err)
	}
	_, err = s.ConsumeOAuth(ctx, "wrong")
	requireErr(t, err, ErrDenied)
	verifier, err := s.ConsumeOAuth(ctx, "state")
	if err != nil || verifier != "verifier" {
		t.Fatalf("OAuth consumption: %q %v", verifier, err)
	}
	_, err = s.ConsumeOAuth(ctx, "state")
	requireErr(t, err, ErrDenied)
	if err := s.SaveOAuth(ctx, "expired-state", "verifier"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE lc_portal_oauth SET expires_at=$1`, now.Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	_, err = s.ConsumeOAuth(ctx, "expired-state")
	requireErr(t, err, ErrDenied)
}

func TestStoreAuthorityRevokesLiveGatewayCapabilities(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	team := mustTeam(t, s, "1")
	p := mustProject(t, s, "1", team.ID, "example/repo")
	mustJoin(t, s, "1", team.ID, "2", "writer")
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Role = "team"
	cfg.ProjectID = p.ID
	cfg.LocalToken = p.Secret
	cfg.DataDir = t.TempDir()
	cfg.MaxBytes = 8 << 20
	cfg.ActionsRepository = p.Repository
	cfg.ActionsDefaultRef = p.DefaultRef
	// Stale configuration must not override the durable authority.
	cfg.TeamMembers = map[string]string{"member": "admin"}
	gateway, err := server.NewProjectGateway(ctx, server.ProjectGatewayConfig{Origin: "https://cache.example", Authority: s})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	if err := gateway.AddProject(ctx, p.ID, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	token, err := access.MintCapabilityToken(p.Secret, access.Claims{Subject: "github-id:2", Project: p.ID, Integration: "turbo", Compatibility: "linux-amd64-schema1", Repository: p.Repository, Ref: p.DefaultRef, DefaultRef: p.DefaultRef, Capabilities: []access.Capability{access.CapabilityRead, access.CapabilityWrite}, ExpiresAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://cache.example/v8/artifacts/test-key", strings.NewReader("artifact"))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set(compatibility.Header, "linux-amd64-schema1")
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodPut); w.Code != 200 {
		t.Fatalf("writer cannot publish: %d %s", w.Code, w.Body.String())
	}
	if err := s.ChangeMember(ctx, "1", team.ID, "2", "reader"); err != nil {
		t.Fatal(err)
	}
	if w := request(http.MethodPut); w.Code < 400 {
		t.Fatalf("downgraded writer can still publish: %d", w.Code)
	}
	if w := request(http.MethodGet); w.Code != 200 || w.Body.String() != "artifact" {
		t.Fatalf("reader cannot restore: %d %s", w.Code, w.Body.String())
	}
	if err := s.ChangeMember(ctx, "1", team.ID, "2", ""); err != nil {
		t.Fatal(err)
	}
	if w := request(http.MethodGet); w.Code < 400 {
		t.Fatalf("removed member can restore with live token: %d", w.Code)
	}
}

// Every contender uses a different team: a team-row lock alone cannot enforce
// this service-wide bound. PostgreSQL may abort a serializable transaction;
// retrying it must observe the winning insert and reject further allocations.
func TestStoreConcurrentGlobalProjectLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := testStore(t)
	type contender struct{ user, team string }
	contenders := make([]contender, 0, 16)
	for i := 0; i < 4; i++ {
		user := fmt.Sprint(i + 1)
		if err := s.UpsertUser(ctx, User{ID: user, Login: "owner-" + user}); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 4; j++ {
			team := mustTeam(t, s, user)
			contenders = append(contenders, contender{user, team.ID})
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 127; i++ {
		_, err := tx.ExecContext(ctx, `INSERT INTO lc_portal_projects(id,team_id,name,repository,default_ref,secret) VALUES($1,$2,'Seed',$3,'refs/heads/main',$4)`, fmt.Sprintf("seed-%d", i), contenders[i%len(contenders)].team, fmt.Sprintf("example/seed%d", i), randomID(""))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, len(contenders))
	var wg sync.WaitGroup
	for i, c := range contenders {
		wg.Add(1)
		go func(i int, c contender) {
			defer wg.Done()
			<-start
			var err error
			for attempts := 0; attempts < 20; attempts++ {
				_, err = s.CreateProject(ctx, c.user, c.team, "Concurrent", fmt.Sprintf("example/concurrent%d", i), "123", "refs/heads/main")
				var dbErr interface{ SQLState() string }
				if !errors.As(err, &dbErr) || dbErr.SQLState() != "40001" {
					break
				}
			}
			results <- err
		}(i, c)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrLimit) {
			t.Fatalf("unexpected concurrent creation result: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("created %d projects with only one global slot", success)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_projects`).Scan(&count); err != nil || count != 128 {
		t.Fatalf("global project count = %d, %v", count, err)
	}
}

func TestStoreConcurrentSessionLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := testStore(t)
	expiry := time.Now().Add(time.Hour).Unix()
	for i := 0; i < 9; i++ {
		if err := s.SaveSession(ctx, fmt.Sprintf("initial-%d", i), Session{User: User{ID: "1"}, CSRF: "csrf", ExpiresAt: expiry}); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results <- s.SaveSession(ctx, fmt.Sprintf("concurrent-%d", i), Session{User: User{ID: "1"}, CSRF: "csrf", ExpiresAt: expiry + int64(i+1)})
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_sessions WHERE user_id='1'`).Scan(&count); err != nil || count != 10 {
		t.Fatalf("concurrent session count = %d, want 10: %v", count, err)
	}
}
