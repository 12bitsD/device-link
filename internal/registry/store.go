package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	// Register pure-Go SQLite driver with database/sql.
	_ "modernc.org/sqlite"
)

const (
	inviteLifetime = 10 * time.Minute
	timeFormat     = "2006-01-02T15:04:05.000000000Z"
)

var (
	ErrUnauthorized = errors.New("registry: unauthorized")
	ErrNotFound     = errors.New("registry: device not found")
)

type Store struct {
	db *sql.DB
}

type Platform struct {
	OS    string
	Arch  string
	Shell string
}

type Device struct {
	ID        int64
	Name      string
	OS        string
	Arch      string
	Shell     string
	ExpiresAt *time.Time
	LastSeen  time.Time
}

type Credential struct {
	Token string
	Name  string
}

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open registry: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close registry: %w", err)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS invites (
 token_hash TEXT PRIMARY KEY, name TEXT NOT NULL, expires_at TEXT NOT NULL,
 device_expires_at TEXT
);
CREATE TABLE IF NOT EXISTS devices (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 token_hash TEXT NOT NULL UNIQUE,
 name TEXT NOT NULL UNIQUE,
 os TEXT NOT NULL,
 arch TEXT NOT NULL,
 shell TEXT NOT NULL,
 expires_at TEXT,
 last_seen TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY,
 device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate registry: %w", err)
	}
	return nil
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func formatOptionalTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(timeFormat)
}

func parseOptionalTime(raw sql.NullString) (*time.Time, error) {
	if !raw.Valid {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func (s *Store) MintControllerToken(ctx context.Context) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	const query = `INSERT INTO settings(key, value) VALUES('controller_token', ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := s.db.ExecContext(ctx, query, hashToken(token)); err != nil {
		return "", fmt.Errorf("store controller token: %w", err)
	}
	return token, nil
}

func (s *Store) HasControllerToken(ctx context.Context, token string) (bool, error) {
	var found string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'controller_token'").Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read controller token: %w", err)
	}
	return token != "" && found == hashToken(token), nil
}

func (s *Store) CreateInvite(ctx context.Context, name string, deviceExpiry *time.Time) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	expires := time.Now().Add(inviteLifetime).UTC().Format(timeFormat)
	const query = "INSERT INTO invites(token_hash, name, expires_at, device_expires_at) VALUES(?, ?, ?, ?)"
	if _, err := s.db.ExecContext(ctx, query, hashToken(token), name, expires, formatOptionalTime(deviceExpiry)); err != nil {
		return "", fmt.Errorf("create invite: %w", err)
	}
	return token, nil
}

func (s *Store) ExchangeInvite(ctx context.Context, code string, platform Platform) (Credential, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Credential{}, fmt.Errorf("begin invite exchange: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	inv, err := consumeInvite(ctx, tx, code)
	if err != nil {
		return Credential{}, err
	}
	credential, err := insertDevice(ctx, tx, inv, platform)
	if err != nil {
		return Credential{}, err
	}
	if err := tx.Commit(); err != nil {
		return Credential{}, fmt.Errorf("commit invite exchange: %w", err)
	}
	return credential, nil
}

type invite struct {
	name         string
	deviceExpiry *time.Time
}

func consumeInvite(ctx context.Context, tx *sql.Tx, code string) (invite, error) {
	var result invite
	var expiresText string
	var deviceExpiry sql.NullString
	const query = "DELETE FROM invites WHERE token_hash = ? RETURNING name, expires_at, device_expires_at"
	err := tx.QueryRowContext(ctx, query, hashToken(code)).Scan(&result.name, &expiresText, &deviceExpiry)
	if errors.Is(err, sql.ErrNoRows) {
		return invite{}, ErrUnauthorized
	}
	if err != nil {
		return invite{}, fmt.Errorf("consume invite: %w", err)
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresText)
	if err != nil {
		return invite{}, fmt.Errorf("parse invite expiry: %w", err)
	}
	if !expires.After(time.Now()) {
		return invite{}, ErrUnauthorized
	}
	result.deviceExpiry, err = parseOptionalTime(deviceExpiry)
	if err != nil {
		return invite{}, fmt.Errorf("parse invited device expiry: %w", err)
	}
	return result, nil
}

func insertDevice(ctx context.Context, tx *sql.Tx, inv invite, platform Platform) (Credential, error) {
	token, err := newToken()
	if err != nil {
		return Credential{}, err
	}
	name, err := availableName(ctx, tx, inv.name)
	if err != nil {
		return Credential{}, err
	}
	now := time.Now().UTC().Format(timeFormat)
	const query = `INSERT INTO devices(token_hash, name, os, arch, shell, expires_at, last_seen)
VALUES(?, ?, ?, ?, ?, ?, ?)`
	_, err = tx.ExecContext(ctx, query, hashToken(token), name, platform.OS, platform.Arch, platform.Shell, formatOptionalTime(inv.deviceExpiry), now)
	if err != nil {
		return Credential{}, fmt.Errorf("insert device: %w", err)
	}
	return Credential{Token: token, Name: name}, nil
}

func availableName(ctx context.Context, tx *sql.Tx, requested string) (string, error) {
	prefix := requested + "-"
	rows, err := tx.QueryContext(ctx, "SELECT name FROM devices WHERE name = ? OR substr(name, 1, ?) = ?", requested, len(prefix), prefix)
	if err != nil {
		return "", fmt.Errorf("check device names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	taken := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", fmt.Errorf("scan device name: %w", err)
		}
		taken[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate device names: %w", err)
	}
	if _, used := taken[requested]; !used {
		return requested, nil
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", requested, suffix)
		if _, used := taken[candidate]; !used {
			return candidate, nil
		}
	}
}

func (s *Store) DeviceByToken(ctx context.Context, token string) (Device, error) {
	const query = `SELECT id, name, os, arch, shell, expires_at, last_seen
FROM devices WHERE token_hash = ?`
	row := s.db.QueryRowContext(ctx, query, hashToken(token))
	device, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrUnauthorized
	}
	if err != nil {
		return Device{}, fmt.Errorf("read device credential: %w", err)
	}
	if device.ExpiresAt != nil && !device.ExpiresAt.After(time.Now()) {
		return Device{}, ErrUnauthorized
	}
	return device, nil
}

func (s *Store) DeviceByName(ctx context.Context, name string) (Device, error) {
	const query = `SELECT id, name, os, arch, shell, expires_at, last_seen
FROM devices WHERE name = ?`
	device, err := scanDevice(s.db.QueryRowContext(ctx, query, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("read device %s: %w", name, err)
	}
	return device, nil
}

func (s *Store) CreateJob(ctx context.Context, deviceID int64) (string, error) {
	raw, err := newToken()
	if err != nil {
		return "", err
	}
	jobID := "job_" + raw[:20]
	const query = "INSERT INTO jobs(id, device_id) SELECT ?, id FROM devices WHERE id = ?"
	result, err := s.db.ExecContext(ctx, query, jobID, deviceID)
	if err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}
	if err := requireOneRow(result); err != nil {
		return "", err
	}
	return jobID, nil
}

func (s *Store) JobDevice(ctx context.Context, jobID string) (Device, error) {
	const query = `SELECT d.id, d.name, d.os, d.arch, d.shell, d.expires_at, d.last_seen
FROM jobs j JOIN devices d ON d.id = j.device_id WHERE j.id = ?`
	device, err := scanDevice(s.db.QueryRowContext(ctx, query, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("read job %s: %w", jobID, err)
	}
	return device, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanDevice(row scanner) (Device, error) {
	var device Device
	var expires sql.NullString
	var lastSeen string
	if err := row.Scan(&device.ID, &device.Name, &device.OS, &device.Arch, &device.Shell, &expires, &lastSeen); err != nil {
		return Device{}, err
	}
	var err error
	device.ExpiresAt, err = parseOptionalTime(expires)
	if err != nil {
		return Device{}, fmt.Errorf("parse device expiry: %w", err)
	}
	device.LastSeen, err = time.Parse(time.RFC3339Nano, lastSeen)
	if err != nil {
		return Device{}, fmt.Errorf("parse device last seen: %w", err)
	}
	return device, nil
}

func (s *Store) List(ctx context.Context) ([]Device, error) {
	const query = `SELECT id, name, os, arch, shell, expires_at, last_seen
FROM devices ORDER BY name`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	devices := make([]Device, 0)
	for rows.Next() {
		device, scanErr := scanDevice(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan device: %w", scanErr)
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate devices: %w", err)
	}
	return devices, nil
}

func (s *Store) Touch(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format(timeFormat)
	const query = "UPDATE devices SET last_seen = ? WHERE id = ? AND (expires_at IS NULL OR expires_at > ?)"
	result, err := s.db.ExecContext(ctx, query, now, id, now)
	if err != nil {
		return fmt.Errorf("touch device: %w", err)
	}
	return requireOneRow(result)
}

func (s *Store) SetExpiry(ctx context.Context, name string, expiry *time.Time) error {
	result, err := s.db.ExecContext(ctx, "UPDATE devices SET expires_at = ? WHERE name = ?", formatOptionalTime(expiry), name)
	if err != nil {
		return fmt.Errorf("set device expiry: %w", err)
	}
	return requireOneRow(result)
}

func (s *Store) Delete(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, "DELETE FROM devices WHERE name = ? RETURNING id", name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("delete device: %w", err)
	}
	return id, nil
}

func requireOneRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check affected device: %w", err)
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}
