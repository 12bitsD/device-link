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
	"strconv"
	"strings"
	"time"

	// Register pure-Go SQLite driver with database/sql.
	_ "modernc.org/sqlite"
)

const inviteLifetime = 10 * time.Minute

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
 requested_name TEXT NOT NULL,
 name TEXT NOT NULL UNIQUE,
 os TEXT NOT NULL,
 arch TEXT NOT NULL,
 shell TEXT NOT NULL,
 expires_at TEXT,
 last_seen TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_devices_requested_name ON devices(requested_name);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate registry: %w", err)
	}
	return nil
}

func Token() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) ControllerToken(ctx context.Context) (string, error) {
	var token string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'controller_token'").Scan(&token)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read controller token: %w", err)
	}
	token, err = Token()
	if err != nil {
		return "", err
	}
	hash := HashToken(token)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES('controller_token', ?)", hash); err != nil {
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
	return token != "" && found == HashToken(token), nil
}

func (s *Store) CreateInvite(ctx context.Context, name string, deviceExpiry *time.Time) (string, error) {
	token, err := Token()
	if err != nil {
		return "", err
	}
	expires := time.Now().Add(inviteLifetime).UTC().Format(time.RFC3339Nano)
	var deviceExpiryText any
	if deviceExpiry != nil {
		deviceExpiryText = deviceExpiry.UTC().Format(time.RFC3339Nano)
	}
	const query = "INSERT INTO invites(token_hash, name, expires_at, device_expires_at) VALUES(?, ?, ?, ?)"
	if _, err := s.db.ExecContext(ctx, query, HashToken(token), name, expires, deviceExpiryText); err != nil {
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
	invite, err := consumeInvite(ctx, tx, code)
	if err != nil {
		return Credential{}, err
	}
	credential, err := insertDevice(ctx, tx, invite, platform)
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
	err := tx.QueryRowContext(ctx, query, HashToken(code)).Scan(&result.name, &expiresText, &deviceExpiry)
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
	if deviceExpiry.Valid {
		parsed, parseErr := time.Parse(time.RFC3339Nano, deviceExpiry.String)
		if parseErr != nil {
			return invite{}, fmt.Errorf("parse invited device expiry: %w", parseErr)
		}
		result.deviceExpiry = &parsed
	}
	return result, nil
}

func insertDevice(ctx context.Context, tx *sql.Tx, inv invite, platform Platform) (Credential, error) {
	token, err := Token()
	if err != nil {
		return Credential{}, err
	}
	name, err := availableName(ctx, tx, inv.name)
	if err != nil {
		return Credential{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var expiry any
	if inv.deviceExpiry != nil {
		expiry = inv.deviceExpiry.UTC().Format(time.RFC3339Nano)
	}
	const query = `INSERT INTO devices(token_hash, requested_name, name, os, arch, shell, expires_at, last_seen)
VALUES(?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = tx.ExecContext(ctx, query, HashToken(token), inv.name, name, platform.OS, platform.Arch, platform.Shell, expiry, now)
	if err != nil {
		return Credential{}, fmt.Errorf("insert device: %w", err)
	}
	return Credential{Token: token, Name: name}, nil
}

func availableName(ctx context.Context, tx *sql.Tx, requested string) (string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT name FROM devices WHERE requested_name = ?", requested)
	if err != nil {
		return "", fmt.Errorf("check device names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	maxSuffix := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", fmt.Errorf("scan device name: %w", err)
		}
		maxSuffix = max(maxSuffix, nameSuffix(name, requested))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate device names: %w", err)
	}
	if maxSuffix == 0 {
		return requested, nil
	}
	return fmt.Sprintf("%s-%d", requested, maxSuffix+1), nil
}

func nameSuffix(name, requested string) int {
	if name == requested {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimPrefix(name, requested+"-"))
	if err != nil || n < 1 {
		return 0
	}
	return n
}

func (s *Store) DeviceByToken(ctx context.Context, token string) (Device, error) {
	const query = `SELECT id, name, os, arch, shell, expires_at, last_seen
FROM devices WHERE token_hash = ?`
	row := s.db.QueryRowContext(ctx, query, HashToken(token))
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

type scanner interface {
	Scan(dest ...any) error
}

func scanDevice(row scanner) (Device, error) {
	var device Device
	var expires sql.NullString
	var lastSeen string
	err := row.Scan(&device.ID, &device.Name, &device.OS, &device.Arch, &device.Shell, &expires, &lastSeen)
	if err != nil {
		return Device{}, err
	}
	if expires.Valid {
		parsed, parseErr := time.Parse(time.RFC3339Nano, expires.String)
		if parseErr != nil {
			return Device{}, fmt.Errorf("parse device expiry: %w", parseErr)
		}
		device.ExpiresAt = &parsed
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, "UPDATE devices SET last_seen = ? WHERE id = ?", now, id)
	if err != nil {
		return fmt.Errorf("touch device: %w", err)
	}
	return requireOneRow(result)
}

func (s *Store) SetExpiry(ctx context.Context, name string, expiry *time.Time) error {
	var value any
	if expiry != nil {
		value = expiry.UTC().Format(time.RFC3339Nano)
	}
	result, err := s.db.ExecContext(ctx, "UPDATE devices SET expires_at = ? WHERE name = ?", value, name)
	if err != nil {
		return fmt.Errorf("set device expiry: %w", err)
	}
	return requireOneRow(result)
}

func (s *Store) Delete(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM devices WHERE name = ?", name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("find device: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM devices WHERE id = ?", id); err != nil {
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

func (s *Store) ExpiredIDs(ctx context.Context) ([]int64, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM devices WHERE expires_at IS NOT NULL AND expires_at <= ?", now)
	if err != nil {
		return nil, fmt.Errorf("list expired devices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired device: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired devices: %w", err)
	}
	return ids, nil
}
