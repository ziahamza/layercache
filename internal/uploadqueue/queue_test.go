package uploadqueue_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/uploadqueue"
)

func TestQueuedUploadSurvivesRestart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "uploads.db")
	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           path,
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatalf("open upload queue: %v", err)
	}

	want := uploadqueue.EnqueueRequest{
		Adapter:  uploadqueue.AdapterTurbo,
		Identity: "0123456789abcdef",
		Digest:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:     128,
		Target: uploadqueue.Target{
			Scope:    uploadqueue.ScopeTeam,
			Endpoint: "https://cache.example.test/v8",
			Project:  "project-123",
		},
	}
	enqueued, err := queue.Enqueue(context.Background(), want)
	if err != nil {
		t.Fatalf("enqueue upload: %v", err)
	}
	if !enqueued.Created {
		t.Fatal("first enqueue did not create a job")
	}
	if err := queue.Close(); err != nil {
		t.Fatalf("close upload queue: %v", err)
	}

	queue, err = uploadqueue.Open(uploadqueue.Config{
		Path:           path,
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatalf("reopen upload queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })

	lease, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim upload after restart: %v", err)
	}
	if lease.Job.ID != enqueued.Job.ID {
		t.Fatalf("claimed job = %q, want %q", lease.Job.ID, enqueued.Job.ID)
	}
	if lease.Job.Adapter != want.Adapter || lease.Job.Identity != want.Identity || lease.Job.Digest != want.Digest || lease.Job.Size != want.Size {
		t.Fatalf("claimed artifact = %#v, want %#v", lease.Job, want)
	}
	if lease.Job.Target != want.Target {
		t.Fatalf("claimed target = %#v, want %#v", lease.Job.Target, want.Target)
	}
	if lease.Job.Attempts != 1 {
		t.Fatalf("claimed attempts = %d, want 1", lease.Job.Attempts)
	}
	if lease.Job.LeaseExpiresAt == nil || !lease.ExpiresAt.Equal(*lease.Job.LeaseExpiresAt) {
		t.Fatalf("lease expiry = %v, job expiry = %v", lease.ExpiresAt, lease.Job.LeaseExpiresAt)
	}
}

func TestUnfinishedReturnsPendingAndLeasedJobsWithoutCompletedJobs(t *testing.T) {
	t.Parallel()

	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })

	completed, err := queue.Enqueue(context.Background(), turboUpload("completed", strings.Repeat("a", 64), 16))
	if err != nil {
		t.Fatal(err)
	}
	completedLease, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Complete(context.Background(), completedLease); err != nil {
		t.Fatal(err)
	}
	leased, err := queue.Enqueue(context.Background(), turboUpload("leased", strings.Repeat("b", 64), 24))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err := queue.Enqueue(context.Background(), turboUpload("pending", strings.Repeat("c", 64), 32))
	if err != nil {
		t.Fatal(err)
	}

	jobs, err := queue.Unfinished(context.Background())
	if err != nil {
		t.Fatalf("list unfinished uploads: %v", err)
	}
	got := make(map[string]uploadqueue.State, len(jobs))
	for _, job := range jobs {
		got[job.ID] = job.State
	}
	if len(got) != 2 || got[leased.Job.ID] != uploadqueue.StateLeased || got[pending.Job.ID] != uploadqueue.StatePending {
		t.Fatalf("unfinished jobs = %#v, want one leased and one pending", jobs)
	}
	if _, exists := got[completed.Job.ID]; exists {
		t.Fatal("completed upload was returned as unfinished")
	}
}

func TestRequestIDMatchesTheDurableEnqueueIdentity(t *testing.T) {
	t.Parallel()

	request := turboUpload("pre-pin", strings.Repeat("d", 64), 48)
	id, err := uploadqueue.RequestID(request)
	if err != nil {
		t.Fatalf("derive upload request ID: %v", err)
	}
	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	enqueued, err := queue.Enqueue(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if enqueued.Job.ID != id {
		t.Fatalf("enqueued job ID = %q, precomputed ID = %q", enqueued.Job.ID, id)
	}
}

func TestCompleteIsFinalAndReleasesQueuedByteQuota(t *testing.T) {
	t.Parallel()

	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 128,
	})
	if err != nil {
		t.Fatalf("open upload queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })

	first, err := queue.Enqueue(context.Background(), turboUpload("first", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 128))
	if err != nil {
		t.Fatalf("enqueue first upload: %v", err)
	}
	lease, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim first upload: %v", err)
	}
	completed, err := queue.Complete(context.Background(), lease)
	if err != nil {
		t.Fatalf("complete first upload: %v", err)
	}
	if completed.State != uploadqueue.StateCompleted || completed.Outcome != uploadqueue.OutcomeUploaded || completed.CompletedAt == nil {
		t.Fatalf("completed job = %#v", completed)
	}
	if _, err := queue.Complete(context.Background(), lease); err != nil {
		t.Fatalf("repeat completion: %v", err)
	}

	duplicate, err := queue.Enqueue(context.Background(), turboUpload("first", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 128))
	if err != nil {
		t.Fatalf("enqueue completed duplicate: %v", err)
	}
	if duplicate.Created || duplicate.Job.ID != first.Job.ID || duplicate.Job.State != uploadqueue.StateCompleted {
		t.Fatalf("completed duplicate = %#v, want existing final job", duplicate)
	}
	if _, err := queue.Enqueue(context.Background(), turboUpload("second", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 128)); err != nil {
		t.Fatalf("enqueue after completion released quota: %v", err)
	}
}

func TestRetryUsesPersistentBoundedExponentialBackoff(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "uploads.db")
	config := uploadqueue.Config{
		Path:           path,
		MaxQueuedBytes: 1024,
		LeaseDuration:  time.Minute,
		BaseBackoff:    2 * time.Second,
		MaxBackoff:     5 * time.Second,
		Now:            clock.Now,
	}
	queue, err := uploadqueue.Open(config)
	if err != nil {
		t.Fatalf("open upload queue: %v", err)
	}
	if _, err := queue.Enqueue(context.Background(), turboUpload("retry", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", 64)); err != nil {
		t.Fatalf("enqueue upload: %v", err)
	}

	first, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim first attempt: %v", err)
	}
	failed, err := queue.Retry(context.Background(), first, "network_timeout")
	if err != nil {
		t.Fatalf("retry first attempt: %v", err)
	}
	if failed.Attempts != 1 || failed.LastError != "network_timeout" {
		t.Fatalf("first failure metadata = %#v", failed)
	}
	if want := clock.Now().Add(2 * time.Second); !failed.NextAttemptAt.Equal(want) {
		t.Fatalf("first retry at %v, want %v", failed.NextAttemptAt, want)
	}
	if err := queue.Close(); err != nil {
		t.Fatalf("close upload queue: %v", err)
	}

	queue, err = uploadqueue.Open(config)
	if err != nil {
		t.Fatalf("reopen upload queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	if _, err := queue.Claim(context.Background()); !errors.Is(err, uploadqueue.ErrNoDueJob) {
		t.Fatalf("claim during backoff error = %v, want ErrNoDueJob", err)
	}

	clock.Advance(2 * time.Second)
	second, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim second attempt: %v", err)
	}
	secondFailure, err := queue.Retry(context.Background(), second, "http_503")
	if err != nil {
		t.Fatalf("retry second attempt: %v", err)
	}
	if want := clock.Now().Add(4 * time.Second); !secondFailure.NextAttemptAt.Equal(want) {
		t.Fatalf("second retry at %v, want %v", secondFailure.NextAttemptAt, want)
	}

	clock.Advance(4 * time.Second)
	third, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim third attempt: %v", err)
	}
	thirdFailure, err := queue.Retry(context.Background(), third, "http_503")
	if err != nil {
		t.Fatalf("retry third attempt: %v", err)
	}
	if want := clock.Now().Add(5 * time.Second); !thirdFailure.NextAttemptAt.Equal(want) {
		t.Fatalf("capped retry at %v, want %v", thirdFailure.NextAttemptAt, want)
	}
}

func TestQueueQuotaRejectsNewWorkWithoutDroppingExistingJobs(t *testing.T) {
	t.Parallel()

	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 100,
	})
	if err != nil {
		t.Fatalf("open upload queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })

	firstRequest := turboUpload("first", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 60)
	first, err := queue.Enqueue(context.Background(), firstRequest)
	if err != nil {
		t.Fatalf("enqueue first upload: %v", err)
	}
	duplicate, err := queue.Enqueue(context.Background(), firstRequest)
	if err != nil {
		t.Fatalf("enqueue duplicate while full: %v", err)
	}
	if duplicate.Created || duplicate.Job.ID != first.Job.ID {
		t.Fatalf("duplicate enqueue = %#v", duplicate)
	}

	if _, err := queue.Enqueue(context.Background(), turboUpload("second", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 41)); !errors.Is(err, uploadqueue.ErrQueueFull) {
		t.Fatalf("overflow enqueue error = %v, want ErrQueueFull", err)
	}
	preserved, err := queue.Get(context.Background(), first.Job.ID)
	if err != nil || preserved.State != uploadqueue.StatePending {
		t.Fatalf("preserved job = %#v, error = %v", preserved, err)
	}

	stats, err := queue.Stats(context.Background())
	if err != nil {
		t.Fatalf("read queue stats: %v", err)
	}
	if stats.QueuedJobs != 1 || stats.QueuedBytes != 60 || stats.MaxQueuedBytes != 100 {
		t.Fatalf("queue stats = %#v", stats)
	}
}

func TestConcurrentWorkersCannotClaimTheSameDueUpload(t *testing.T) {
	t.Parallel()

	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatalf("open upload queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	if _, err := queue.Enqueue(context.Background(), turboUpload("contended", "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", 64)); err != nil {
		t.Fatalf("enqueue upload: %v", err)
	}

	const workers = 32
	start := make(chan struct{})
	results := make(chan error, workers)
	leases := make(chan uploadqueue.Lease, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			lease, claimErr := queue.Claim(context.Background())
			if claimErr == nil {
				leases <- lease
			}
			results <- claimErr
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(leases)

	claimed := 0
	noWork := 0
	for claimErr := range results {
		switch {
		case claimErr == nil:
			claimed++
		case errors.Is(claimErr, uploadqueue.ErrNoDueJob):
			noWork++
		default:
			t.Fatalf("concurrent claim error: %v", claimErr)
		}
	}
	if claimed != 1 || noWork != workers-1 {
		t.Fatalf("claims = %d, no-work = %d, want 1 and %d", claimed, noWork, workers-1)
	}
	lease := <-leases
	if _, err := queue.Complete(context.Background(), lease); err != nil {
		t.Fatalf("complete claimed upload: %v", err)
	}
}

func TestSeparateQueueConnectionsCannotClaimTheSameUpload(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "uploads.db")
	config := uploadqueue.Config{Path: path, MaxQueuedBytes: 1024}
	first, err := uploadqueue.Open(config)
	if err != nil {
		t.Fatalf("open first upload queue: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := uploadqueue.Open(config)
	if err != nil {
		t.Fatalf("open second upload queue: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := first.Enqueue(context.Background(), turboUpload("multiprocess", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 64)); err != nil {
		t.Fatalf("enqueue upload: %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, queue := range []*uploadqueue.Queue{first, second} {
		go func(queue *uploadqueue.Queue) {
			<-start
			_, claimErr := queue.Claim(context.Background())
			results <- claimErr
		}(queue)
	}
	close(start)
	claimed := 0
	noWork := 0
	for range 2 {
		claimErr := <-results
		switch {
		case claimErr == nil:
			claimed++
		case errors.Is(claimErr, uploadqueue.ErrNoDueJob):
			noWork++
		default:
			t.Fatalf("claim across queue connections: %v", claimErr)
		}
	}
	if claimed != 1 || noWork != 1 {
		t.Fatalf("claims = %d, no-work = %d, want 1 each", claimed, noWork)
	}
}

func TestExpiredLeaseCanBeReclaimedAndRejectsTheStaleWorker(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2026, time.August, 30, 13, 0, 0, 0, time.UTC))
	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 1024,
		LeaseDuration:  5 * time.Second,
		Now:            clock.Now,
	})
	if err != nil {
		t.Fatalf("open upload queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	if _, err := queue.Enqueue(context.Background(), turboUpload("reclaim", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", 64)); err != nil {
		t.Fatalf("enqueue upload: %v", err)
	}

	stale, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim initial lease: %v", err)
	}
	clock.Advance(6 * time.Second)
	current, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatalf("reclaim expired lease: %v", err)
	}
	if current.Generation != stale.Generation+1 || current.Job.Attempts != 2 {
		t.Fatalf("reclaimed lease = %#v, stale = %#v", current, stale)
	}
	if _, err := queue.Complete(context.Background(), stale); !errors.Is(err, uploadqueue.ErrLeaseLost) {
		t.Fatalf("stale completion error = %v, want ErrLeaseLost", err)
	}
	if _, err := queue.Complete(context.Background(), current); err != nil {
		t.Fatalf("complete reclaimed upload: %v", err)
	}
}

func TestConcurrentDuplicateEnqueueCreatesOneJob(t *testing.T) {
	t.Parallel()

	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 64,
	})
	if err != nil {
		t.Fatalf("open upload queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	request := turboUpload("duplicate", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 64)

	const writers = 32
	start := make(chan struct{})
	results := make(chan uploadqueue.EnqueueResult, writers)
	errorsChannel := make(chan error, writers)
	var group sync.WaitGroup
	for range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, enqueueErr := queue.Enqueue(context.Background(), request)
			results <- result
			errorsChannel <- enqueueErr
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsChannel)

	for enqueueErr := range errorsChannel {
		if enqueueErr != nil {
			t.Fatalf("concurrent enqueue: %v", enqueueErr)
		}
	}
	created := 0
	var id string
	for result := range results {
		if result.Created {
			created++
		}
		if id == "" {
			id = result.Job.ID
		} else if result.Job.ID != id {
			t.Fatalf("job ID = %q, want %q", result.Job.ID, id)
		}
	}
	if created != 1 {
		t.Fatalf("created count = %d, want 1", created)
	}
	stats, err := queue.Stats(context.Background())
	if err != nil {
		t.Fatalf("read queue stats: %v", err)
	}
	if stats.QueuedJobs != 1 || stats.QueuedBytes != 64 {
		t.Fatalf("queue stats = %#v", stats)
	}
}

func TestConflictingArtifactForOneTargetIdentityIsRejected(t *testing.T) {
	t.Parallel()

	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatalf("open upload queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	if _, err := queue.Enqueue(context.Background(), turboUpload("immutable", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 64)); err != nil {
		t.Fatalf("enqueue first artifact: %v", err)
	}
	if _, err := queue.Enqueue(context.Background(), turboUpload("immutable", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 64)); !errors.Is(err, uploadqueue.ErrConflict) {
		t.Fatalf("conflicting enqueue error = %v, want ErrConflict", err)
	}
}

func TestQueueRejectsCredentialBearingTargetsAndWorkspacePaths(t *testing.T) {
	t.Parallel()

	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatalf("open upload queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })

	tests := []struct {
		name     string
		mutate   func(*uploadqueue.EnqueueRequest)
		wantText string
	}{
		{
			name: "userinfo",
			mutate: func(request *uploadqueue.EnqueueRequest) {
				request.Target.Endpoint = "https://bearer:do-not-persist@cache.example.test/v8"
			},
			wantText: "credentials",
		},
		{
			name: "query token",
			mutate: func(request *uploadqueue.EnqueueRequest) {
				request.Target.Endpoint = "https://cache.example.test/v8?token=do-not-persist"
			},
			wantText: "query",
		},
		{
			name: "fragment",
			mutate: func(request *uploadqueue.EnqueueRequest) {
				request.Target.Endpoint = "https://cache.example.test/v8#do-not-persist"
			},
			wantText: "fragment",
		},
		{
			name: "unix workspace path",
			mutate: func(request *uploadqueue.EnqueueRequest) {
				request.Identity = "/home/engineer/repository/output"
			},
			wantText: "workspace path",
		},
		{
			name: "windows workspace path",
			mutate: func(request *uploadqueue.EnqueueRequest) {
				request.Identity = `C:\\Users\\engineer\\repository\\output`
			},
			wantText: "workspace path",
		},
		{
			name: "workspace path as project",
			mutate: func(request *uploadqueue.EnqueueRequest) {
				request.Target.Project = "/home/engineer/repository"
			},
			wantText: "workspace path",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := turboUpload("safe-identity", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 64)
			test.mutate(&request)
			if _, err := queue.Enqueue(context.Background(), request); err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("enqueue error = %v, want text %q", err, test.wantText)
			}
		})
	}
}

func TestRetryAcceptsOnlyNonSensitiveErrorCodes(t *testing.T) {
	t.Parallel()

	queue, err := uploadqueue.Open(uploadqueue.Config{
		Path:           filepath.Join(t.TempDir(), "uploads.db"),
		MaxQueuedBytes: 1024,
	})
	if err != nil {
		t.Fatalf("open upload queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	if _, err := queue.Enqueue(context.Background(), turboUpload("failure", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 64)); err != nil {
		t.Fatalf("enqueue upload: %v", err)
	}
	lease, err := queue.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim upload: %v", err)
	}
	if _, err := queue.Retry(context.Background(), lease, "Bearer do-not-persist"); err == nil {
		t.Fatal("credential-shaped failure message was accepted")
	}
	job, err := queue.Get(context.Background(), lease.Job.ID)
	if err != nil {
		t.Fatalf("read leased job: %v", err)
	}
	if job.State != uploadqueue.StateLeased || job.LastError != "" {
		t.Fatalf("job after rejected failure metadata = %#v", job)
	}
}

func turboUpload(identity, digest string, size int64) uploadqueue.EnqueueRequest {
	return uploadqueue.EnqueueRequest{
		Adapter:  uploadqueue.AdapterTurbo,
		Identity: identity,
		Digest:   digest,
		Size:     size,
		Target: uploadqueue.Target{
			Scope:    uploadqueue.ScopeTeam,
			Endpoint: "https://cache.example.test/v8",
			Project:  "project-123",
		},
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}
