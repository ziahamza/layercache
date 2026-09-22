package portal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

var (
	ErrDenied   = errors.New("access denied")
	ErrConflict = errors.New("resource already exists or last administrator would be removed")
	ErrLimit    = errors.New("service capacity reached")
	ErrInvalid  = errors.New("invalid input")
)

type User struct {
	ID    string `json:"id"`
	Login string `json:"login"`
}
type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}
type Project struct {
	ID         string `json:"id"`
	TeamID     string `json:"teamId"`
	Name       string `json:"name"`
	Repository string `json:"repository"`
	DefaultRef string `json:"defaultRef"`
	Role       string `json:"role,omitempty"`
	Secret     string `json:"-"`
}
type Member struct {
	UserID string `json:"userId"`
	Login  string `json:"login"`
	Role   string `json:"role"`
}
type Invitation struct {
	ID        string `json:"id"`
	TeamID    string `json:"teamId"`
	TeamName  string `json:"teamName"`
	Role      string `json:"role"`
	ExpiresAt int64  `json:"expiresAt"`
}
type Session struct {
	User           User
	CSRF           string
	EncryptedToken string
	ExpiresAt      int64
}

// Store is the single durable membership authority for managed projects. Team
// mutations lock the team row, including last-admin checks and invitation accepts.
type Store struct{ db *sql.DB }

func OpenStore(ctx context.Context, postgresURL, filename string) (*Store, error) {
	driver, source := "pgx", postgresURL
	if postgresURL == "" {
		if !filepath.IsAbs(filename) {
			return nil, errors.New("portal database path must be absolute")
		}
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			return nil, err
		}
		driver, source = "sqlite", filename
	}
	db, err := sql.Open(driver, source)
	if err != nil {
		return nil, errors.New("open portal database")
	}
	db.SetMaxOpenConns(4)
	store := &Store{db: db}
	fail := func() (*Store, error) { db.Close(); return nil, errors.New("initialize portal database") }
	if driver == "sqlite" {
		db.SetMaxOpenConns(1)
		for _, statement := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON"} {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return fail()
			}
		}
		if err := os.Chmod(filename, 0600); err != nil {
			return fail()
		}
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS lc_portal_users (id TEXT PRIMARY KEY, login TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lc_portal_teams (id TEXT PRIMARY KEY, name TEXT NOT NULL, created_by TEXT NOT NULL, revision BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS lc_portal_members (team_id TEXT NOT NULL REFERENCES lc_portal_teams(id), user_id TEXT NOT NULL REFERENCES lc_portal_users(id), role TEXT NOT NULL CHECK(role IN ('admin','writer','reader')), PRIMARY KEY(team_id,user_id))`,
		`CREATE TABLE IF NOT EXISTS lc_portal_projects (id TEXT PRIMARY KEY, team_id TEXT NOT NULL REFERENCES lc_portal_teams(id), name TEXT NOT NULL, repository TEXT NOT NULL, default_ref TEXT NOT NULL, secret TEXT NOT NULL, UNIQUE(team_id,repository))`,
		`CREATE TABLE IF NOT EXISTS lc_portal_invites (id TEXT PRIMARY KEY, team_id TEXT NOT NULL REFERENCES lc_portal_teams(id), user_id TEXT NOT NULL, role TEXT NOT NULL, expires_at BIGINT NOT NULL, UNIQUE(team_id,user_id))`,
		`CREATE TABLE IF NOT EXISTS lc_portal_sessions (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES lc_portal_users(id), csrf TEXT NOT NULL, token TEXT NOT NULL, expires_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS lc_portal_sessions_user ON lc_portal_sessions(user_id)`,
		`CREATE TABLE IF NOT EXISTS lc_portal_oauth (id TEXT PRIMARY KEY, verifier TEXT NOT NULL, expires_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lc_portal_audit (id TEXT PRIMARY KEY, team_id TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, subject TEXT NOT NULL, created_at BIGINT NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fail()
		}
	}
	return store, nil
}
func (s *Store) Close() error { return s.db.Close() }
func randomID(prefix string) string {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("system random source unavailable")
	}
	return prefix + hex.EncodeToString(value[:])
}
func tokenHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
func validName(name string) bool {
	return strings.TrimSpace(name) == name && len(name) > 0 && len(name) <= 80 && !strings.ContainsAny(name, "\x00\r\n")
}
func validRole(role string) bool { return role == "admin" || role == "writer" || role == "reader" }
func (s *Store) UpsertUser(ctx context.Context, user User) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO lc_portal_users(id,login) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET login=excluded.login`, user.ID, user.Login)
	return err
}
func (s *Store) Teams(ctx context.Context, user string) ([]Team, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id,t.name,m.role FROM lc_portal_teams t JOIN lc_portal_members m ON m.team_id=t.id WHERE m.user_id=$1 ORDER BY t.name,t.id`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Team{}
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.Role); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}
func (s *Store) CreateTeam(ctx context.Context, user, name string) (Team, error) {
	if !validName(name) {
		return Team{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Team{}, err
	}
	defer tx.Rollback()
	var total, owned int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_teams`).Scan(&total); err != nil {
		return Team{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_teams WHERE created_by=$1`, user).Scan(&owned); err != nil {
		return Team{}, err
	}
	if total >= 128 || owned >= 5 {
		return Team{}, ErrLimit
	}
	t := Team{ID: randomID("team-"), Name: name, Role: "admin"}
	if _, err = tx.ExecContext(ctx, `INSERT INTO lc_portal_teams(id,name,created_by) VALUES($1,$2,$3)`, t.ID, t.Name, user); err != nil {
		return Team{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO lc_portal_members(team_id,user_id,role) VALUES($1,$2,'admin')`, t.ID, user); err != nil {
		return Team{}, err
	}
	if err = audit(ctx, tx, t.ID, user, "team.create", t.ID); err != nil {
		return Team{}, err
	}
	return t, tx.Commit()
}
func teamRole(ctx context.Context, tx *sql.Tx, team, user string) (string, error) {
	// UPDATE takes a row lock in PostgreSQL and a write lock in SQLite before
	// reading membership, serializing authorization and last-administrator checks.
	result, err := tx.ExecContext(ctx, `UPDATE lc_portal_teams SET revision=revision+1 WHERE id=$1`, team)
	if err != nil {
		return "", err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return "", ErrDenied
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM lc_portal_members WHERE team_id=$1 AND user_id=$2`, team, user).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrDenied
	}
	return role, err
}
func audit(ctx context.Context, tx *sql.Tx, team, actor, action, subject string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO lc_portal_audit(id,team_id,actor,action,subject,created_at) VALUES($1,$2,$3,$4,$5,$6)`, randomID("event-"), team, actor, action, subject, time.Now().Unix())
	return err
}
func (s *Store) Projects(ctx context.Context, user string) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,p.team_id,p.name,p.repository,p.default_ref,p.secret,m.role FROM lc_portal_projects p JOIN lc_portal_members m ON m.team_id=p.team_id WHERE m.user_id=$1 ORDER BY p.name,p.id`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.TeamID, &p.Name, &p.Repository, &p.DefaultRef, &p.Secret, &p.Role); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
func (s *Store) AllProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,team_id,name,repository,default_ref,secret FROM lc_portal_projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.TeamID, &p.Name, &p.Repository, &p.DefaultRef, &p.Secret); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
func (s *Store) CreateProject(ctx context.Context, user, team, name, repository, defaultRef string) (Project, error) {
	repository = strings.ToLower(repository)
	if !validName(name) || repository == "" || !strings.HasPrefix(defaultRef, "refs/heads/") {
		return Project{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()
	role, err := teamRole(ctx, tx, team, user)
	if err != nil {
		return Project{}, err
	}
	if role != "admin" {
		return Project{}, ErrDenied
	}
	var total, teamCount int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_projects`).Scan(&total); err != nil {
		return Project{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_projects WHERE team_id=$1`, team).Scan(&teamCount); err != nil {
		return Project{}, err
	}
	if total >= 128 || teamCount >= 10 {
		return Project{}, ErrLimit
	}
	// Serializable isolation also protects the global count across different teams.
	p := Project{ID: randomID("project-"), TeamID: team, Name: name, Repository: repository, DefaultRef: defaultRef, Secret: randomID(""), Role: "admin"}
	result, err := tx.ExecContext(ctx, `INSERT INTO lc_portal_projects(id,team_id,name,repository,default_ref,secret) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(team_id,repository) DO NOTHING`, p.ID, p.TeamID, p.Name, p.Repository, p.DefaultRef, p.Secret)
	if err != nil {
		return Project{}, err
	}
	if count, err := result.RowsAffected(); err != nil {
		return Project{}, err
	} else if count == 0 {
		return Project{}, ErrConflict
	}
	if err = audit(ctx, tx, team, user, "project.create", p.ID); err != nil {
		return Project{}, err
	}
	return p, tx.Commit()
}
func (s *Store) MemberRole(ctx context.Context, project, subject string) (string, error) {
	if !strings.HasPrefix(subject, "github-id:") {
		return "", ErrDenied
	}
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT m.role FROM lc_portal_members m JOIN lc_portal_projects p ON p.team_id=m.team_id WHERE p.id=$1 AND m.user_id=$2`, project, strings.TrimPrefix(subject, "github-id:")).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrDenied
	}
	return role, err
}
func (s *Store) Members(ctx context.Context, user, team string) ([]Member, error) {
	var allowed int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_members WHERE team_id=$1 AND user_id=$2`, team, user).Scan(&allowed); err != nil {
		return nil, err
	}
	if allowed != 1 {
		return nil, ErrDenied
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.login,m.role FROM lc_portal_members m JOIN lc_portal_users u ON u.id=m.user_id WHERE m.team_id=$1 ORDER BY u.login`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Login, &m.Role); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
func (s *Store) ChangeMember(ctx context.Context, user, team, target, role string) error {
	if role != "" && !validRole(role) {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actorRole, err := teamRole(ctx, tx, team, user)
	if err != nil {
		return err
	}
	if actorRole != "admin" {
		return ErrDenied
	}
	var previous string
	if err = tx.QueryRowContext(ctx, `SELECT role FROM lc_portal_members WHERE team_id=$1 AND user_id=$2`, team, target).Scan(&previous); err != nil {
		return ErrDenied
	}
	if previous == "admin" && role != "admin" {
		var admins int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_members WHERE team_id=$1 AND role='admin'`, team).Scan(&admins); err != nil {
			return err
		}
		if admins <= 1 {
			return ErrConflict
		}
	}
	if role == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM lc_portal_members WHERE team_id=$1 AND user_id=$2`, team, target)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE lc_portal_members SET role=$1 WHERE team_id=$2 AND user_id=$3`, role, team, target)
	}
	if err != nil {
		return err
	}
	// Old invitations must never restore privileges after removal or demotion.
	if _, err = tx.ExecContext(ctx, `DELETE FROM lc_portal_invites WHERE team_id=$1 AND user_id=$2`, team, target); err != nil {
		return err
	}
	if err = audit(ctx, tx, team, user, "member.change", target+":"+role); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Invite(ctx context.Context, user, team, target, role string) (Invitation, error) {
	if !validRole(role) || target == "" {
		return Invitation{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Invitation{}, err
	}
	defer tx.Rollback()
	actorRole, err := teamRole(ctx, tx, team, user)
	if err != nil {
		return Invitation{}, err
	}
	if actorRole != "admin" {
		return Invitation{}, ErrDenied
	}
	var existing int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_members WHERE team_id=$1 AND user_id=$2`, team, target).Scan(&existing); err != nil {
		return Invitation{}, err
	}
	if existing != 0 {
		return Invitation{}, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM lc_portal_invites WHERE expires_at<$1`, time.Now().Unix()); err != nil {
		return Invitation{}, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_invites WHERE team_id=$1`, team).Scan(&count); err != nil {
		return Invitation{}, err
	}
	if count >= 100 {
		return Invitation{}, ErrLimit
	}
	inv := Invitation{ID: randomID("invite-"), TeamID: team, Role: role, ExpiresAt: time.Now().Add(7 * 24 * time.Hour).Unix()}
	if _, err = tx.ExecContext(ctx, `INSERT INTO lc_portal_invites(id,team_id,user_id,role,expires_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(team_id,user_id) DO UPDATE SET id=excluded.id,role=excluded.role,expires_at=excluded.expires_at`, inv.ID, team, target, role, inv.ExpiresAt); err != nil {
		return Invitation{}, err
	}
	if err = audit(ctx, tx, team, user, "member.invite", target+":"+role); err != nil {
		return Invitation{}, err
	}
	return inv, tx.Commit()
}
func (s *Store) Invitations(ctx context.Context, user string) ([]Invitation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id,i.team_id,t.name,i.role,i.expires_at FROM lc_portal_invites i JOIN lc_portal_teams t ON t.id=i.team_id WHERE i.user_id=$1 AND i.expires_at>$2 ORDER BY i.id`, user, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Invitation{}
	for rows.Next() {
		var i Invitation
		if err := rows.Scan(&i.ID, &i.TeamID, &i.TeamName, &i.Role, &i.ExpiresAt); err != nil {
			return nil, err
		}
		result = append(result, i)
	}
	return result, rows.Err()
}
func (s *Store) Accept(ctx context.Context, user, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var team, role string
	if err = tx.QueryRowContext(ctx, `SELECT team_id,role FROM lc_portal_invites WHERE id=$1 AND user_id=$2 AND expires_at>$3`, id, user, time.Now().Unix()).Scan(&team, &role); err != nil {
		return ErrDenied
	}
	if _, err = tx.ExecContext(ctx, `UPDATE lc_portal_teams SET revision=revision+1 WHERE id=$1`, team); err != nil {
		return err
	}
	// Recheck after taking the team lock: invite replacement/revocation wins.
	if err = tx.QueryRowContext(ctx, `SELECT role FROM lc_portal_invites WHERE id=$1 AND user_id=$2 AND expires_at>$3`, id, user, time.Now().Unix()).Scan(&role); err != nil {
		return ErrDenied
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO lc_portal_members(team_id,user_id,role) VALUES($1,$2,$3) ON CONFLICT(team_id,user_id) DO NOTHING`, team, user, role); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM lc_portal_invites WHERE id=$1`, id); err != nil {
		return err
	}
	if err = audit(ctx, tx, team, user, "member.accept", user); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) SaveSession(ctx context.Context, token string, session Session) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize simultaneous browser logins for this user before trimming sessions.
	// A per-process mutex would not protect two service instances sharing PostgreSQL.
	result, err := tx.ExecContext(ctx, `UPDATE lc_portal_users SET login=login WHERE id=$1`, session.User.ID)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return ErrDenied
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM lc_portal_sessions WHERE expires_at<$1`, time.Now().Unix()); err != nil {
		return err
	}
	// Keep the nine newest sessions before inserting the tenth; this also heals
	// any excess sessions left by an earlier concurrent login race.
	if _, err = tx.ExecContext(ctx, `DELETE FROM lc_portal_sessions WHERE user_id=$1 AND id NOT IN (SELECT id FROM lc_portal_sessions WHERE user_id=$1 ORDER BY expires_at DESC,id LIMIT 9)`, session.User.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO lc_portal_sessions(id,user_id,csrf,token,expires_at) VALUES($1,$2,$3,$4,$5)`, tokenHash(token), session.User.ID, session.CSRF, session.EncryptedToken, session.ExpiresAt); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Session(ctx context.Context, token string) (Session, error) {
	var v Session
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.login,s.csrf,s.token,s.expires_at FROM lc_portal_sessions s JOIN lc_portal_users u ON u.id=s.user_id WHERE s.id=$1 AND s.expires_at>$2`, tokenHash(token), time.Now().Unix()).Scan(&v.User.ID, &v.User.Login, &v.CSRF, &v.EncryptedToken, &v.ExpiresAt)
	if err != nil {
		return Session{}, ErrDenied
	}
	return v, nil
}
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM lc_portal_sessions WHERE id=$1`, tokenHash(token))
	return err
}
func (s *Store) SaveOAuth(ctx context.Context, state, verifier string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM lc_portal_oauth WHERE expires_at<$1`, time.Now().Unix()); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lc_portal_oauth`).Scan(&count); err != nil {
		return err
	}
	if count >= 1000 {
		return ErrLimit
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO lc_portal_oauth(id,verifier,expires_at) VALUES($1,$2,$3)`, tokenHash(state), verifier, time.Now().Add(10*time.Minute).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ConsumeOAuth(ctx context.Context, state string) (string, error) {
	var verifier string
	err := s.db.QueryRowContext(ctx, `DELETE FROM lc_portal_oauth WHERE id=$1 AND expires_at>$2 RETURNING verifier`, tokenHash(state), time.Now().Unix()).Scan(&verifier)
	if err != nil {
		return "", ErrDenied
	}
	return verifier, nil
}

func userID(id int64) string { return fmt.Sprint(id) }
