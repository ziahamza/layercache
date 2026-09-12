package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/compatibility"
	"github.com/layercache/layercache/internal/remote"
	"github.com/layercache/layercache/internal/uploadqueue"
)

const teamUploadPinNamespace = "team-upload"
const turboUploadIdentityPrefix = "turbo1."

type preparedTurboUpload struct {
	request uploadqueue.EnqueueRequest
	pin     artifact.Pin
}

func (server *Server) openUploadQueue() error {
	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(server.config.DataDir, "team-uploads.db"),
		MaxQueuedBytes: server.config.MaxBytes,
	})
	if err != nil {
		return err
	}
	server.uploads = queue
	if err := server.reconcileUploadPins(context.Background()); err != nil {
		_ = queue.Close()
		server.uploads = nil
		return err
	}
	server.uploadWake = make(chan struct{}, 1)
	server.uploadDone = make(chan struct{})
	server.uploadContext, server.cancelUploads = context.WithCancel(context.Background())
	go server.runUploadWorker()
	return nil
}

func (server *Server) closeUploadQueue() {
	if server.cancelUploads != nil {
		server.cancelUploads()
		if server.uploadDone != nil {
			<-server.uploadDone
		}
	}
	if server.uploads != nil {
		_ = server.uploads.Close()
	}
}

func (server *Server) enqueueTurboUpload(ctx context.Context, key artifact.Key, entry artifact.Entry) error {
	if server.uploads == nil {
		return errors.New("Team Cache upload queue is unavailable")
	}
	prepared, err := server.pinTurboUpload(ctx, key, entry)
	if err != nil {
		return err
	}
	return server.enqueuePinnedTurboUpload(ctx, prepared)
}

func (server *Server) pinTurboUpload(ctx context.Context, key artifact.Key, entry artifact.Entry) (preparedTurboUpload, error) {
	request := uploadqueue.EnqueueRequest{
		Adapter: uploadqueue.AdapterTurbo,
		Target: uploadqueue.Target{
			Scope: uploadqueue.ScopeTeam, Endpoint: server.config.TeamURL, Project: key.Project,
		},
		Identity: encodeTurboUploadIdentity(key.Compatibility, key.Native),
		Digest:   entry.Digest,
		Size:     entry.Size,
	}
	owner, err := uploadqueue.RequestID(request)
	if err != nil {
		return preparedTurboUpload{}, err
	}
	prepared := preparedTurboUpload{
		request: request,
		pin: artifact.Pin{
			Owner: owner, Key: key, Digest: entry.Digest, Size: entry.Size,
		},
	}
	if err := server.store.Pin(ctx, teamUploadPinNamespace, prepared.pin); err != nil {
		return preparedTurboUpload{}, err
	}
	return prepared, nil
}

func (server *Server) enqueuePinnedTurboUpload(ctx context.Context, prepared preparedTurboUpload) error {
	if server.uploads == nil {
		server.releaseTurboUploadPin(prepared.pin.Owner)
		return errors.New("Team Cache upload queue is unavailable")
	}
	result, err := server.uploads.Enqueue(ctx, prepared.request)
	if err != nil {
		// The pin is owned by this enqueue attempt until a durable queue row
		// exists. Any failure, including cancellation or SQLite errors, must
		// release it so the artifact remains evictable.
		server.releaseTurboUploadPin(prepared.pin.Owner)
		return err
	}
	if result.Job.State == uploadqueue.StateCompleted {
		server.releaseTurboUploadPin(prepared.pin.Owner)
		return nil
	}
	select {
	case server.uploadWake <- struct{}{}:
	default:
	}
	return nil
}

func (server *Server) reconcileUploadPins(ctx context.Context) error {
	jobs, err := server.uploads.Unfinished(ctx)
	if err != nil {
		return err
	}
	pins := make([]artifact.Pin, 0, len(jobs))
	for _, job := range jobs {
		if job.Adapter != uploadqueue.AdapterTurbo {
			continue
		}
		compatibilityID, nativeKey, err := decodeTurboUploadIdentity(job.Identity, server.config.CompatibilityID)
		if err != nil {
			return err
		}
		pins = append(pins, artifact.Pin{
			Owner: job.ID,
			Key: artifact.Key{
				Integration: "turbo", Project: job.Target.Project,
				Compatibility: compatibilityID, Native: nativeKey,
			},
			Digest: job.Digest,
			Size:   job.Size,
		})
	}
	_, err = server.store.ReconcilePins(ctx, teamUploadPinNamespace, pins)
	return err
}

func encodeTurboUploadIdentity(compatibilityID, nativeKey string) string {
	return turboUploadIdentityPrefix +
		base64.RawURLEncoding.EncodeToString([]byte(compatibilityID)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(nativeKey))
}

func decodeTurboUploadIdentity(identity, legacyCompatibility string) (string, string, error) {
	if !strings.HasPrefix(identity, turboUploadIdentityPrefix) {
		return legacyCompatibility, identity, nil
	}
	encoded := strings.TrimPrefix(identity, turboUploadIdentityPrefix)
	parts := strings.Split(encoded, ".")
	if len(parts) != 2 {
		return "", "", errors.New("queued Turbo identity is malformed")
	}
	compatibilityBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", fmt.Errorf("decode queued Turbo compatibility: %w", err)
	}
	nativeBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("decode queued Turbo key: %w", err)
	}
	if len(compatibilityBytes) == 0 || len(nativeBytes) == 0 {
		return "", "", errors.New("queued Turbo identity is incomplete")
	}
	compatibilityID := string(compatibilityBytes)
	if err := compatibility.Validate(compatibilityID); err != nil {
		return "", "", fmt.Errorf("validate queued Turbo compatibility: %w", err)
	}
	return compatibilityID, string(nativeBytes), nil
}

func (server *Server) releaseTurboUploadPin(owner string) {
	if server.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.store.Unpin(ctx, teamUploadPinNamespace, owner)
}

func (server *Server) runUploadWorker() {
	defer close(server.uploadDone)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		server.drainDueUploads()
		select {
		case <-server.uploadContext.Done():
			return
		case <-server.uploadWake:
		case <-ticker.C:
		}
	}
}

func (server *Server) drainDueUploads() {
	for {
		if server.uploadContext.Err() != nil {
			return
		}
		lease, err := server.claimUpload()
		if errors.Is(err, uploadqueue.ErrNoDueJob) {
			return
		}
		if err != nil {
			return
		}
		if !server.processClaimedUpload(lease) {
			return
		}
	}
}

func (server *Server) processClaimedUpload(lease uploadqueue.Lease) bool {
	if server.uploadContext.Err() != nil {
		server.retryUpload(lease, "runtime_stopping")
		return false
	}
	server.processUpload(lease)
	return true
}

func (server *Server) processUpload(lease uploadqueue.Lease) {
	if lease.Job.Adapter != uploadqueue.AdapterTurbo ||
		lease.Job.Target.Endpoint != server.config.TeamURL ||
		lease.Job.Target.Project != server.config.ProjectID {
		server.retryUpload(lease, "target_mismatch")
		return
	}
	compatibilityID, nativeKey, err := decodeTurboUploadIdentity(lease.Job.Identity, server.config.CompatibilityID)
	if err != nil {
		server.retryUpload(lease, "identity_invalid")
		return
	}
	key := artifact.Key{
		Integration: "turbo", Project: server.config.ProjectID,
		Compatibility: compatibilityID, Native: nativeKey,
	}
	entry, file, err := server.store.Get(server.uploadContext, key)
	if err != nil {
		server.retryUpload(lease, "artifact_unavailable")
		return
	}
	defer file.Close()
	if entry.Digest != lease.Job.Digest || entry.Size != lease.Job.Size {
		server.retryUpload(lease, "artifact_mismatch")
		return
	}
	team := server.team
	if compatibilityID != server.config.CompatibilityID {
		team, err = remote.NewTurboClientForCompatibilityWithTimeouts(server.config.TeamURL, server.currentTeamToken(), compatibilityID, remote.Timeouts{
			Metadata: server.config.RemoteMetadataTimeout, TransferIdle: server.config.RemoteTransferIdleTimeout,
		})
		if err != nil {
			server.retryUpload(lease, "identity_invalid")
			return
		}
	}
	if err := team.Put(server.uploadContext, key.Native, entry, file); err != nil {
		server.retryUpload(lease, uploadFailureCode(err))
		return
	}
	server.completeUpload(lease)
}

func (server *Server) claimUpload() (uploadqueue.Lease, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(server.uploadContext), 2*time.Second)
	defer cancel()
	return server.uploads.Claim(ctx)
}

func (server *Server) completeUpload(lease uploadqueue.Lease) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(server.uploadContext), 2*time.Second)
	defer cancel()
	job, err := server.uploads.Complete(ctx, lease)
	if err == nil {
		server.releaseTurboUploadPin(job.ID)
	}
}

// retryUpload must survive runtime cancellation. Otherwise a worker that owns
// a lease while the daemon is stopping leaves the job unavailable until the
// full lease timeout elapses after restart.
func (server *Server) retryUpload(lease uploadqueue.Lease, failureCode string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(server.uploadContext), 2*time.Second)
	defer cancel()
	_, _ = server.uploads.Retry(ctx, lease, failureCode)
}

func uploadFailureCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "network_timeout"
	case errors.Is(err, remote.ErrUnauthorized):
		return "unauthorized"
	default:
		return "remote_unavailable"
	}
}
