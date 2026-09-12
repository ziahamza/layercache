package cloud

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"
)

// AcquirePromotion serializes mutation of one BuildKit branch reference. The
// raw lease token is returned once and PostgreSQL stores only its SHA-256 hash.
func (store *Store) AcquirePromotion(
	ctx context.Context,
	project string,
	reference string,
	owner string,
	duration time.Duration,
) (PromotionLease, error) {
	if project != store.config.Project {
		return PromotionLease{}, errors.New("promotion lease is outside the configured project")
	}
	if err := validateIdentity("promotion reference", reference, 1024); err != nil {
		return PromotionLease{}, err
	}
	if err := validateIdentity("promotion owner", owner, 512); err != nil {
		return PromotionLease{}, err
	}
	duration, err := promotionLeaseDuration(duration)
	if err != nil {
		return PromotionLease{}, err
	}
	token, err := randomToken("promotion-")
	if err != nil {
		return PromotionLease{}, err
	}
	tokenHash := sha256.Sum256([]byte(token))
	now := store.config.Now().UTC()
	lease := PromotionLease{
		Project: project, Reference: reference, Owner: owner, Token: token, ExpiresAt: now.Add(duration),
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return PromotionLease{}, store.safeError("begin BuildKit promotion lease", err)
	}
	defer transaction.Rollback()
	var storedReference string
	err = transaction.QueryRowContext(ctx, `
		INSERT INTO layercache_promotion_leases_v1(
			project_id, reference, owner, token_hash, acquired_at, expires_at
		) VALUES($1, $2, $3, $4, $5, $6)
		ON CONFLICT(project_id, reference) DO UPDATE
		SET owner = EXCLUDED.owner,
			token_hash = EXCLUDED.token_hash,
			acquired_at = EXCLUDED.acquired_at,
			expires_at = EXCLUDED.expires_at
		WHERE layercache_promotion_leases_v1.expires_at <= $5
		RETURNING reference`,
		project, reference, owner, tokenHash[:], now, lease.ExpiresAt).Scan(&storedReference)
	if errors.Is(err, sql.ErrNoRows) {
		return PromotionLease{}, ErrLeaseHeld
	}
	if err != nil {
		return PromotionLease{}, store.safeError("acquire BuildKit promotion lease", err)
	}
	if err := store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: project, Actor: owner, Action: "buildkit.promotion.acquire",
		Resource: "oci:" + reference, Outcome: "allowed",
		Attributes: map[string]string{"expiresAt": lease.ExpiresAt.Format(time.RFC3339Nano)}, CreatedAt: now,
	}); err != nil {
		return PromotionLease{}, err
	}
	if err := transaction.Commit(); err != nil {
		return PromotionLease{}, store.safeError("commit BuildKit promotion lease", err)
	}
	return lease, nil
}

// RenewPromotion extends only the still-live lease identified by its raw
// token and original owner. It cannot resurrect an expired lease or extend a
// replacement acquired by another writer.
func (store *Store) RenewPromotion(
	ctx context.Context,
	lease PromotionLease,
	duration time.Duration,
) (PromotionLease, error) {
	if lease.Project != store.config.Project {
		return PromotionLease{}, errors.New("promotion lease is outside the configured project")
	}
	if err := validateIdentity("promotion reference", lease.Reference, 1024); err != nil {
		return PromotionLease{}, err
	}
	if err := validateIdentity("promotion owner", lease.Owner, 512); err != nil {
		return PromotionLease{}, err
	}
	if lease.Token == "" {
		return PromotionLease{}, errors.New("promotion lease token is required")
	}
	duration, err := promotionLeaseDuration(duration)
	if err != nil {
		return PromotionLease{}, err
	}
	providedHash := sha256.Sum256([]byte(lease.Token))
	now := store.config.Now().UTC()
	expiresAt := now.Add(duration)
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return PromotionLease{}, store.safeError("begin BuildKit promotion renewal", err)
	}
	defer transaction.Rollback()
	var storedHash []byte
	var storedOwner string
	var storedExpiry time.Time
	err = transaction.QueryRowContext(ctx, `
		SELECT token_hash, owner, expires_at
		FROM layercache_promotion_leases_v1
		WHERE project_id = $1 AND reference = $2
		FOR UPDATE`, lease.Project, lease.Reference).
		Scan(&storedHash, &storedOwner, &storedExpiry)
	if errors.Is(err, sql.ErrNoRows) {
		return PromotionLease{}, ErrLeaseLost
	}
	if err != nil {
		return PromotionLease{}, store.safeError("read BuildKit promotion lease for renewal", err)
	}
	if storedOwner != lease.Owner || !storedExpiry.After(now) || len(storedHash) != sha256.Size ||
		subtle.ConstantTimeCompare(storedHash, providedHash[:]) != 1 {
		return PromotionLease{}, ErrLeaseLost
	}
	result, err := transaction.ExecContext(ctx, `
		UPDATE layercache_promotion_leases_v1
		SET expires_at = $4
		WHERE project_id = $1 AND reference = $2 AND owner = $3
			AND token_hash = $5 AND expires_at > $6`,
		lease.Project, lease.Reference, lease.Owner, expiresAt, providedHash[:], now)
	if err != nil {
		return PromotionLease{}, store.safeError("renew BuildKit promotion lease", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return PromotionLease{}, store.safeError("confirm BuildKit promotion renewal", err)
	}
	if updated != 1 {
		return PromotionLease{}, ErrLeaseLost
	}
	if err := store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: lease.Project, Actor: lease.Owner, Action: "buildkit.promotion.renew",
		Resource: "oci:" + lease.Reference, Outcome: "allowed",
		Attributes: map[string]string{"expiresAt": expiresAt.Format(time.RFC3339Nano)}, CreatedAt: now,
	}); err != nil {
		return PromotionLease{}, err
	}
	if err := transaction.Commit(); err != nil {
		return PromotionLease{}, store.safeError("commit BuildKit promotion renewal", err)
	}
	lease.ExpiresAt = expiresAt
	return lease, nil
}

// ReleasePromotion is safe to repeat. A stale token cannot release a newer
// lease for the same reference.
func (store *Store) ReleasePromotion(ctx context.Context, lease PromotionLease) error {
	if lease.Project != store.config.Project {
		return errors.New("promotion lease is outside the configured project")
	}
	if err := validateIdentity("promotion reference", lease.Reference, 1024); err != nil {
		return err
	}
	if err := validateIdentity("promotion owner", lease.Owner, 512); err != nil {
		return err
	}
	if lease.Token == "" {
		return errors.New("promotion lease token is required")
	}
	providedHash := sha256.Sum256([]byte(lease.Token))
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return store.safeError("begin BuildKit promotion release", err)
	}
	defer transaction.Rollback()
	var storedHash []byte
	var storedOwner string
	err = transaction.QueryRowContext(ctx, `
		SELECT token_hash, owner FROM layercache_promotion_leases_v1
		WHERE project_id = $1 AND reference = $2 FOR UPDATE`, lease.Project, lease.Reference).
		Scan(&storedHash, &storedOwner)
	if errors.Is(err, sql.ErrNoRows) {
		// A repeated release has already achieved the requested state.
		return nil
	}
	if err != nil {
		return store.safeError("read BuildKit promotion lease", err)
	}
	if storedOwner != lease.Owner || len(storedHash) != sha256.Size || subtle.ConstantTimeCompare(storedHash, providedHash[:]) != 1 {
		return ErrLeaseLost
	}
	result, err := transaction.ExecContext(ctx, `
		DELETE FROM layercache_promotion_leases_v1
		WHERE project_id = $1 AND reference = $2 AND token_hash = $3`,
		lease.Project, lease.Reference, providedHash[:])
	if err != nil {
		return store.safeError("release BuildKit promotion lease", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return store.safeError("confirm BuildKit promotion release", err)
	}
	if deleted != 1 {
		return ErrLeaseLost
	}
	if err := store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: lease.Project, Actor: lease.Owner, Action: "buildkit.promotion.release",
		Resource: "oci:" + lease.Reference, Outcome: "allowed", CreatedAt: store.config.Now().UTC(),
	}); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return store.safeError("commit BuildKit promotion release", err)
	}
	return nil
}

func promotionLeaseDuration(duration time.Duration) (time.Duration, error) {
	if duration == 0 {
		duration = defaultLeaseTTL
	}
	if duration < 5*time.Second || duration > 15*time.Minute {
		return 0, errors.New("promotion lease duration must be between five seconds and fifteen minutes")
	}
	return duration, nil
}
