package cloud

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// AppendAudit records one security-relevant outcome. Callers must never pass a
// bearer token, connection string, raw cache path, or environment value in an
// event. The database rejects update and delete operations on this table.
func (store *Store) AppendAudit(ctx context.Context, event AuditEvent) (AuditEvent, error) {
	if event.Project == "" {
		event.Project = store.config.Project
	}
	if err := store.validateAudit(event); err != nil {
		return AuditEvent{}, err
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = store.config.Now().UTC()
	} else {
		event.CreatedAt = event.CreatedAt.UTC()
	}
	attributes, err := json.Marshal(event.Attributes)
	if err != nil {
		return AuditEvent{}, fmt.Errorf("encode cloud audit attributes: %w", err)
	}
	if len(attributes) > 16<<10 {
		return AuditEvent{}, errors.New("cloud audit attributes exceed 16 KiB")
	}
	err = store.database.QueryRowContext(ctx, `
		INSERT INTO layercache_audit_events_v1(
			project_id, actor, action, resource, outcome, attributes_json, created_at
		) VALUES($1, $2, $3, $4, $5, $6::jsonb, $7)
		RETURNING event_id`,
		event.Project, event.Actor, event.Action, event.Resource, event.Outcome, string(attributes), event.CreatedAt).
		Scan(&event.ID)
	if err != nil {
		return AuditEvent{}, store.safeError("append cloud audit event", err)
	}
	return event, nil
}

// ReadAudit returns events for the configured project only. AfterID is an
// exclusive cursor. A zero limit uses 100 and the maximum is 1000.
func (store *Store) ReadAudit(ctx context.Context, query AuditQuery) ([]AuditEvent, error) {
	if query.Project == "" {
		query.Project = store.config.Project
	}
	if query.Project != store.config.Project {
		return nil, errors.New("cloud audit query is outside the configured project")
	}
	if query.AfterID < 0 {
		return nil, errors.New("cloud audit cursor cannot be negative")
	}
	if query.Limit == 0 {
		query.Limit = 100
	}
	if query.Limit < 1 || query.Limit > 1000 {
		return nil, errors.New("cloud audit limit must be between 1 and 1000")
	}
	rows, err := store.database.QueryContext(ctx, `
		SELECT event_id, project_id, actor, action, resource, outcome, attributes_json, created_at
		FROM layercache_audit_events_v1
		WHERE project_id = $1 AND event_id > $2
		ORDER BY event_id
		LIMIT $3`, query.Project, query.AfterID, query.Limit)
	if err != nil {
		return nil, store.safeError("read cloud audit events", err)
	}
	defer rows.Close()
	events := make([]AuditEvent, 0, query.Limit)
	for rows.Next() {
		var event AuditEvent
		var attributes []byte
		if err := rows.Scan(
			&event.ID, &event.Project, &event.Actor, &event.Action, &event.Resource,
			&event.Outcome, &attributes, &event.CreatedAt,
		); err != nil {
			return nil, store.safeError("decode cloud audit event", err)
		}
		if err := json.Unmarshal(attributes, &event.Attributes); err != nil {
			return nil, fmt.Errorf("decode cloud audit attributes: %w", err)
		}
		event.CreatedAt = event.CreatedAt.UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, store.safeError("iterate cloud audit events", err)
	}
	return events, nil
}

func (store *Store) validateAudit(event AuditEvent) error {
	if event.Project != store.config.Project {
		return errors.New("cloud audit event is outside the configured project")
	}
	for name, value := range map[string]string{
		"audit actor": event.Actor, "audit action": event.Action,
		"audit resource": event.Resource, "audit outcome": event.Outcome,
	} {
		if err := validateIdentity(name, value, 512); err != nil {
			return err
		}
	}
	if len(event.Attributes) > 64 {
		return errors.New("cloud audit event has too many attributes")
	}
	for key, value := range event.Attributes {
		if err := validateIdentity("audit attribute name", key, 128); err != nil {
			return err
		}
		if len(value) > 2048 || strings.IndexFunc(value, func(character rune) bool {
			return character == 0 || character == '\r' || character == '\n'
		}) >= 0 {
			return errors.New("cloud audit attribute value is invalid")
		}
	}
	return nil
}

func (store *Store) SetMember(ctx context.Context, actor string, member Member) (Member, error) {
	if member.Project == "" {
		member.Project = store.config.Project
	}
	if member.Project != store.config.Project {
		return Member{}, errors.New("cloud membership is outside the configured project")
	}
	if err := validateIdentity("membership actor", actor, 512); err != nil {
		return Member{}, err
	}
	if err := validateIdentity("membership subject", member.Subject, 512); err != nil {
		return Member{}, err
	}
	if member.Role != "reader" && member.Role != "writer" && member.Role != "admin" {
		return Member{}, errors.New("cloud membership role must be reader, writer, or admin")
	}
	member.UpdatedAt = store.config.Now().UTC()
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return Member{}, store.safeError("begin cloud membership change", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO layercache_memberships_v1(project_id, subject, role, updated_at)
		VALUES($1, $2, $3, $4)
		ON CONFLICT(project_id, subject) DO UPDATE
		SET role = EXCLUDED.role, updated_at = EXCLUDED.updated_at`,
		member.Project, member.Subject, member.Role, member.UpdatedAt); err != nil {
		return Member{}, store.safeError("record cloud membership", err)
	}
	if err := store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: member.Project, Actor: actor, Action: "membership.set",
		Resource: "subject:" + member.Subject, Outcome: "allowed",
		Attributes: map[string]string{"role": member.Role}, CreatedAt: member.UpdatedAt,
	}); err != nil {
		return Member{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Member{}, store.safeError("commit cloud membership change", err)
	}
	return member, nil
}

func (store *Store) RemoveMember(ctx context.Context, actor, subject string) error {
	if err := validateIdentity("membership actor", actor, 512); err != nil {
		return err
	}
	if err := validateIdentity("membership subject", subject, 512); err != nil {
		return err
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return store.safeError("begin cloud membership removal", err)
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(ctx, `
		DELETE FROM layercache_memberships_v1 WHERE project_id = $1 AND subject = $2`,
		store.config.Project, subject)
	if err != nil {
		return store.safeError("remove cloud membership", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return store.safeError("confirm cloud membership removal", err)
	}
	if removed == 0 {
		return ErrNotFound
	}
	if err := store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: store.config.Project, Actor: actor, Action: "membership.remove",
		Resource: "subject:" + subject, Outcome: "allowed", CreatedAt: store.config.Now().UTC(),
	}); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return store.safeError("commit cloud membership removal", err)
	}
	return nil
}

// UpsertMembers atomically applies configured memberships without removing
// other project members. Public servers use this operation because a Team
// server may own the full membership set in the same cloud backend.
func (store *Store) UpsertMembers(ctx context.Context, actor string, members []Member) error {
	return store.reconcileMembers(ctx, actor, members, false)
}

// ReconcileMembers atomically replaces the configured project's membership
// set. Team servers use it during startup so configuration role changes and
// removals take effect before the server accepts authentication requests.
func (store *Store) ReconcileMembers(ctx context.Context, actor string, members []Member) error {
	return store.reconcileMembers(ctx, actor, members, true)
}

func (store *Store) reconcileMembers(ctx context.Context, actor string, members []Member, removeMissing bool) error {
	if err := validateIdentity("membership actor", actor, 512); err != nil {
		return err
	}
	desired := make(map[string]string, len(members))
	for _, member := range members {
		if member.Project == "" {
			member.Project = store.config.Project
		}
		if member.Project != store.config.Project {
			return errors.New("cloud membership is outside the configured project")
		}
		if err := validateIdentity("membership subject", member.Subject, 512); err != nil {
			return err
		}
		if member.Role != "reader" && member.Role != "writer" && member.Role != "admin" {
			return errors.New("cloud membership role must be reader, writer, or admin")
		}
		if _, duplicate := desired[member.Subject]; duplicate {
			return fmt.Errorf("duplicate cloud membership subject %q", member.Subject)
		}
		desired[member.Subject] = member.Role
	}

	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return store.safeError("begin cloud membership reconciliation", err)
	}
	defer transaction.Rollback()
	rows, err := transaction.QueryContext(ctx, `
		SELECT subject, role
		FROM layercache_memberships_v1
		WHERE project_id = $1
		FOR UPDATE`, store.config.Project)
	if err != nil {
		return store.safeError("read cloud memberships for reconciliation", err)
	}
	current := make(map[string]string)
	for rows.Next() {
		var subject, role string
		if err := rows.Scan(&subject, &role); err != nil {
			_ = rows.Close()
			return store.safeError("decode cloud membership for reconciliation", err)
		}
		current[subject] = role
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return store.safeError("iterate cloud memberships for reconciliation", err)
	}
	if err := rows.Close(); err != nil {
		return store.safeError("close cloud membership reconciliation query", err)
	}

	now := store.config.Now().UTC()
	desiredSubjects := make([]string, 0, len(desired))
	for subject := range desired {
		desiredSubjects = append(desiredSubjects, subject)
	}
	sort.Strings(desiredSubjects)
	for _, subject := range desiredSubjects {
		role := desired[subject]
		if current[subject] == role {
			continue
		}
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO layercache_memberships_v1(project_id, subject, role, updated_at)
			VALUES($1, $2, $3, $4)
			ON CONFLICT(project_id, subject) DO UPDATE
			SET role = EXCLUDED.role, updated_at = EXCLUDED.updated_at`,
			store.config.Project, subject, role, now); err != nil {
			return store.safeError("reconcile configured cloud membership", err)
		}
		if err := store.appendAuditTx(ctx, transaction, AuditEvent{
			Project: store.config.Project, Actor: actor, Action: "membership.set",
			Resource: "subject:" + subject, Outcome: "allowed",
			Attributes: map[string]string{"role": role}, CreatedAt: now,
		}); err != nil {
			return err
		}
	}

	if removeMissing {
		removedSubjects := make([]string, 0, len(current))
		for subject := range current {
			if _, retained := desired[subject]; !retained {
				removedSubjects = append(removedSubjects, subject)
			}
		}
		sort.Strings(removedSubjects)
		for _, subject := range removedSubjects {
			if _, err := transaction.ExecContext(ctx, `
			DELETE FROM layercache_memberships_v1
			WHERE project_id = $1 AND subject = $2`, store.config.Project, subject); err != nil {
				return store.safeError("remove unconfigured cloud membership", err)
			}
			if err := store.appendAuditTx(ctx, transaction, AuditEvent{
				Project: store.config.Project, Actor: actor, Action: "membership.remove",
				Resource: "subject:" + subject, Outcome: "allowed", CreatedAt: now,
			}); err != nil {
				return err
			}
		}
	}
	if err := transaction.Commit(); err != nil {
		return store.safeError("commit cloud membership reconciliation", err)
	}
	return nil
}

func (store *Store) MemberRole(ctx context.Context, project, subject string) (string, error) {
	if project != store.config.Project {
		return "", ErrNotFound
	}
	if err := validateIdentity("membership subject", subject, 512); err != nil {
		return "", err
	}
	var role string
	err := store.database.QueryRowContext(ctx, `
		SELECT role FROM layercache_memberships_v1 WHERE project_id = $1 AND subject = $2`,
		project, subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", store.safeError("read cloud membership", err)
	}
	return role, nil
}

func (store *Store) appendAuditTx(ctx context.Context, transaction *sql.Tx, event AuditEvent) error {
	if err := store.validateAudit(event); err != nil {
		return err
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = store.config.Now().UTC()
	}
	attributes, err := json.Marshal(event.Attributes)
	if err != nil {
		return err
	}
	if len(attributes) > 16<<10 {
		return errors.New("cloud audit attributes exceed 16 KiB")
	}
	_, err = transaction.ExecContext(ctx, `
		INSERT INTO layercache_audit_events_v1(
			project_id, actor, action, resource, outcome, attributes_json, created_at
		) VALUES($1, $2, $3, $4, $5, $6::jsonb, $7)`,
		event.Project, event.Actor, event.Action, event.Resource, event.Outcome,
		string(attributes), event.CreatedAt.UTC())
	if err != nil {
		return store.safeError("append cloud audit event", err)
	}
	return nil
}
