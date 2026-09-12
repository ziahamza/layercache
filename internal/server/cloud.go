package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/cloud"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publictrust"
)

const (
	cloudMaintenanceInterval = time.Minute
	cloudMaintenanceTimeout  = 30 * time.Second
	cloudAuditTimeout        = 2 * time.Second
)

type cacheStore interface {
	Put(context.Context, artifact.Key, artifact.Metadata, io.Reader) (artifact.Entry, bool, error)
	PutVerified(context.Context, artifact.Key, artifact.Metadata, io.Reader, string) (artifact.Entry, bool, error)
	Get(context.Context, artifact.Key) (artifact.Entry, io.ReadCloser, error)
	Head(context.Context, artifact.Key) (artifact.Entry, error)
	Stats(context.Context) (artifact.Stats, error)
	Delete(context.Context, artifact.Key) error
	GC(context.Context, int64) error
	Pin(context.Context, string, artifact.Pin) error
	Unpin(context.Context, string, string) error
	ReconcilePins(context.Context, string, []artifact.Pin) (int, error)
	Close() error
}

type publicationRegistry interface {
	Publish(context.Context, publictrust.Publication) error
	MarkAmbiguous(context.Context, string, string) error
	Resolve(context.Context, string) (publictrust.Publication, error)
	FindBuild(context.Context, publictrust.BuildIdentity) (publictrust.Publication, error)
	ActivePublications(context.Context) ([]publictrust.Publication, error)
	Retire(context.Context, string, string, string, time.Time) error
	Revoke(context.Context, string, string, time.Time) error
	Close() error
}

type publicationActorRegistry interface {
	PublishAs(context.Context, string, publictrust.Publication) error
	RevokeAs(context.Context, string, string, string, time.Time) error
}

type actionsStorage interface {
	actionscache.StorageIndex
	Close() error
}

type actionsTeamPublicationStatsReader interface {
	TeamPublicationStats(context.Context) (actionscache.TeamPublicationStats, error)
}

type cloudMembershipStore interface {
	UpsertMembers(context.Context, string, []cloud.Member) error
	ReconcileMembers(context.Context, string, []cloud.Member) error
}

func cloudStoreConfig(cfg config.Config) cloud.Config {
	return cloud.Config{
		PostgresURL:    cfg.CloudPostgresURL,
		S3Endpoint:     cfg.CloudS3Endpoint,
		S3Bucket:       cfg.CloudS3Bucket,
		S3Region:       cfg.CloudS3Region,
		S3AccessKey:    cfg.CloudS3AccessKey,
		S3SecretKey:    cfg.CloudS3SecretKey,
		S3PathStyle:    cfg.CloudS3UsePathStyle,
		Project:        cfg.ProjectID,
		MaxBytes:       cfg.MaxBytes,
		EvictionPolicy: cfg.EvictionPolicy,
		StageTTL:       cfg.CloudStageTTL,
		BlobGrace:      cfg.CloudBlobGrace,
	}
}

func (server *Server) applyConfiguredCloudMemberships(ctx context.Context, memberships cloudMembershipStore) error {
	members := make([]cloud.Member, 0, len(server.config.TeamMembers))
	for login, role := range server.config.TeamMembers {
		members = append(members, cloud.Member{
			Project: server.config.ProjectID,
			Subject: "github:" + strings.ToLower(strings.TrimSpace(login)),
			Role:    strings.ToLower(role),
		})
	}
	switch server.config.Role {
	case "team":
		return memberships.ReconcileMembers(ctx, "configuration-bootstrap", members)
	case "public":
		return memberships.UpsertMembers(ctx, "configuration-bootstrap", members)
	default:
		return nil
	}
}

func (server *Server) startCloudMaintenance() {
	ctx, cancel := context.WithCancel(context.Background())
	server.cloudCancel = cancel
	server.cloudDone = make(chan struct{})
	server.cloudHealthy = true
	go func() {
		defer close(server.cloudDone)
		ticker := time.NewTicker(cloudMaintenanceInterval)
		defer ticker.Stop()
		for {
			server.runCloudMaintenancePass(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (server *Server) runCloudMaintenancePass(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, cloudMaintenanceTimeout)
	_, err := server.cloudStore.Maintain(ctx)
	if err == nil && server.cloudActions != nil {
		_, err = server.cloudActions.Maintain(ctx)
	}
	cancel()
	server.cloudHealthMu.Lock()
	server.cloudHealthy = err == nil
	server.cloudHealthMu.Unlock()
}

func (server *Server) stopCloudMaintenance() {
	if server.cloudCancel == nil {
		return
	}
	server.cloudCancel()
	if server.cloudDone != nil {
		<-server.cloudDone
	}
	server.cloudCancel = nil
}

func (server *Server) cloudMaintenanceHealthy() bool {
	server.cloudHealthMu.RLock()
	defer server.cloudHealthMu.RUnlock()
	return server.cloudHealthy
}

func (server *Server) cloudRoutes() {
	if server.cloudStore == nil {
		return
	}
	server.cacheAdministrationRoutes()
	server.mux.Handle("GET /v1/audit", server.requireToken(http.HandlerFunc(server.readAudit)))
	server.mux.Handle("PUT /v1/members/{subject}", server.requireToken(http.HandlerFunc(server.setMember)))
	server.mux.Handle("DELETE /v1/members/{subject}", server.requireToken(http.HandlerFunc(server.removeMember)))
	server.mux.Handle("POST /v1/buildkit/promotion-leases/acquire", server.requireToken(http.HandlerFunc(server.acquirePromotionLease)))
	server.mux.Handle("POST /v1/buildkit/promotion-leases/renew", server.requireToken(http.HandlerFunc(server.renewPromotionLease)))
	server.mux.Handle("POST /v1/buildkit/promotion-leases/release", server.requireToken(http.HandlerFunc(server.releasePromotionLease)))
}

func (server *Server) readAudit(writer http.ResponseWriter, request *http.Request) {
	after, err := parseBoundedInteger(request.URL.Query().Get("after"), 0, 1<<62)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid audit cursor"})
		return
	}
	limit, err := parseBoundedInteger(request.URL.Query().Get("limit"), 100, 1000)
	if err != nil || limit == 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid audit limit"})
		return
	}
	events, err := server.cloudStore.ReadAudit(request.Context(), cloud.AuditQuery{
		Project: server.config.ProjectID, AfterID: after, Limit: int(limit),
	})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "read audit events"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"events": events})
}

func parseBoundedInteger(value string, defaultValue, maximum int64) (int64, error) {
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 || parsed > maximum {
		return 0, errors.New("integer is out of range")
	}
	return parsed, nil
}

func (server *Server) setMember(writer http.ResponseWriter, request *http.Request) {
	subject, ok := canonicalMemberSubject(request.PathValue("subject"))
	if !ok {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid membership subject"})
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := decodeCloudJSON(writer, request, &body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid membership change"})
		return
	}
	member, err := server.cloudStore.SetMember(request.Context(), requestActor(request), cloud.Member{
		Project: server.config.ProjectID, Subject: subject, Role: strings.ToLower(body.Role),
	})
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid membership change"})
		return
	}
	writeJSON(writer, http.StatusOK, member)
}

func (server *Server) removeMember(writer http.ResponseWriter, request *http.Request) {
	subject, ok := canonicalMemberSubject(request.PathValue("subject"))
	if !ok {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid membership subject"})
		return
	}
	err := server.cloudStore.RemoveMember(request.Context(), requestActor(request), subject)
	if errors.Is(err, cloud.ErrNotFound) {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "remove project member"})
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func canonicalMemberSubject(subject string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(subject), "github:") {
		return "", false
	}
	login := strings.ToLower(strings.TrimSpace(subject[len("github:"):]))
	if login == "" || len(login) > 39 || login[0] == '-' || login[len(login)-1] == '-' || strings.Contains(login, "--") {
		return "", false
	}
	for _, character := range login {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return "", false
		}
	}
	return "github:" + login, true
}

type promotionLeaseRequest struct {
	Reference  string `json:"reference"`
	LeaseToken string `json:"leaseToken,omitempty"`
}

func (server *Server) acquirePromotionLease(writer http.ResponseWriter, request *http.Request) {
	var body promotionLeaseRequest
	if err := decodeCloudJSON(writer, request, &body); err != nil || body.LeaseToken != "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid promotion lease request"})
		return
	}
	lease, err := server.cloudStore.AcquirePromotion(
		request.Context(), server.config.ProjectID, body.Reference, requestActor(request), 0,
	)
	if errors.Is(err, cloud.ErrLeaseHeld) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "promotion reference is busy"})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid promotion lease request"})
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"leaseToken": lease.Token, "expiresAt": lease.ExpiresAt,
	})
}

func (server *Server) releasePromotionLease(writer http.ResponseWriter, request *http.Request) {
	var body promotionLeaseRequest
	if err := decodeCloudJSON(writer, request, &body); err != nil || body.LeaseToken == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid promotion lease release"})
		return
	}
	err := server.cloudStore.ReleasePromotion(request.Context(), cloud.PromotionLease{
		Project: server.config.ProjectID, Reference: body.Reference,
		Owner: requestActor(request), Token: body.LeaseToken,
	})
	if errors.Is(err, cloud.ErrLeaseLost) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "promotion lease is no longer active"})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid promotion lease release"})
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) renewPromotionLease(writer http.ResponseWriter, request *http.Request) {
	var body promotionLeaseRequest
	if err := decodeCloudJSON(writer, request, &body); err != nil || body.LeaseToken == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid promotion lease renewal"})
		return
	}
	lease, err := server.cloudStore.RenewPromotion(request.Context(), cloud.PromotionLease{
		Project: server.config.ProjectID, Reference: body.Reference,
		Owner: requestActor(request), Token: body.LeaseToken,
	}, 0)
	if errors.Is(err, cloud.ErrLeaseLost) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "promotion lease is no longer active"})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid promotion lease renewal"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"leaseToken": lease.Token, "expiresAt": lease.ExpiresAt,
	})
}

func decodeCloudJSON(writer http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	defer request.Body.Close()
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return rejectTrailingJSON(decoder)
}

func requestActor(request *http.Request) string {
	claims, ok := access.ClaimsFromContext(request.Context())
	if !ok || claims.Subject == "" {
		return "authenticated-client"
	}
	return claims.Subject
}

func (server *Server) recordAuthorizationDenial(request *http.Request) {
	if server.cloudStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudAuditTimeout)
	defer cancel()
	_, _ = server.cloudStore.AppendAudit(ctx, cloud.AuditEvent{
		Project: server.config.ProjectID, Actor: "unauthenticated",
		Action: "authorization.denied", Resource: auditRoute(request), Outcome: "denied",
		Attributes: map[string]string{"method": request.Method},
	})
}

func auditRoute(request *http.Request) string {
	path := request.URL.Path
	switch {
	case strings.HasPrefix(path, "/v8/artifacts/"):
		return "route:turbo.artifact"
	case strings.HasPrefix(path, "/_apis/artifactcache/"), strings.HasPrefix(path, "/_layercache/compatibility/"):
		return "route:actions.cache"
	case strings.HasPrefix(path, "/v1/public-build"):
		return "route:public-build"
	case strings.HasPrefix(path, "/v1/public/"):
		return "route:public-cache"
	case strings.HasPrefix(path, "/v1/buildkit/promotion-leases/"):
		return "route:buildkit.promotion"
	case strings.HasPrefix(path, "/v1/members"):
		return "route:membership"
	case strings.HasPrefix(path, "/v1/audit"):
		return "route:audit"
	default:
		return "route:administration"
	}
}

var _ cacheStore = (*artifact.Store)(nil)
var _ cacheStore = (*cloud.Store)(nil)
var _ publicationRegistry = (*publictrust.Registry)(nil)
var _ publicationRegistry = (*cloud.PublicationRegistry)(nil)
var _ actionsStorage = (*actionscache.PersistentStorage)(nil)
var _ actionsStorage = (*cloud.ActionsStorage)(nil)

// Server protects cloudHealthy because the maintenance goroutine updates it
// while status requests read it.
type cloudHealthLock struct{ sync.RWMutex }
