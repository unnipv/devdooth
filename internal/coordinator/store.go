package coordinator

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
)

// sqliteUniqueViolation codes: SQLITE_CONSTRAINT_UNIQUE (2067) and
// SQLITE_CONSTRAINT_PRIMARYKEY (1555).
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		code := se.Code()
		return code == 2067 || code == 1555
	}
	return false
}

// Sentinel errors let callers map storage outcomes to HTTP status codes without
// leaking database detail.
var (
	ErrInvalidEnrollToken = errors.New("invalid enrollment token")
	ErrEnrollTokenUsed    = errors.New("enrollment token has already been used")
	ErrEnrollTokenExpired = errors.New("enrollment token has expired")
	ErrDeviceNameTaken    = errors.New("device name is already enrolled")
	ErrUnknownDeviceToken = errors.New("unknown device token")
	ErrDeviceRevoked      = errors.New("device has been revoked")
	ErrDeviceNotFound     = errors.New("no such device")
)

// Store is the coordinator's durable metadata: enrolled devices and
// single-use enrollment tokens. Browser sessions and profiles are never
// stored here; those live on workers.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Device is an enrolled worker identity.
type Device struct {
	ID        string
	Name      string
	CreatedAt time.Time
	LastSeen  time.Time
	Revoked   bool
}

// EnrollToken is a one-time credential used to enroll a device.
type EnrollToken struct {
	ID        string
	Label     string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time
}

// OpenStore opens (and migrates) the coordinator database. An empty path opens
// an in-memory database, which is useful for tests and ephemeral runs.
func OpenStore(path string) (*Store, error) {
	dsn := path
	if path == "" {
		// Unique per store so concurrent in-memory stores never share tables.
		dsn = "file:devdooth-" + randomToken(8) + "?mode=memory&cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite writes serialize; one connection avoids "database is locked".
	db.SetMaxOpenConns(1)
	if path != "" {
		if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS devices (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	token_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL DEFAULT 0,
	revoked    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS enroll_tokens (
	id         TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	label      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER NOT NULL DEFAULT 0
);
`)
	return err
}

// hashToken hashes a high-entropy opaque token for storage. Tokens are 256-bit
// random values, so a plain SHA-256 is sufficient; no password stretching is
// needed.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateEnrollToken mints a single-use enrollment token and returns its
// plaintext exactly once.
func (s *Store) CreateEnrollToken(label string, ttl time.Duration) (id, token string, expires time.Time, err error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	id = "ent_" + randomToken(9)
	token = randomToken(32)
	now := s.now()
	expires = now.Add(ttl)
	_, err = s.db.Exec(
		`INSERT INTO enroll_tokens (id, token_hash, label, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		id, hashToken(token), label, now.Unix(), expires.Unix(),
	)
	return id, token, expires, err
}

// RedeemEnrollToken consumes a token and creates a device. Redemption is
// atomic and single-use: a concurrent or replayed redemption fails.
func (s *Store) RedeemEnrollToken(token, deviceName string) (*Device, string, error) {
	if token == "" || deviceName == "" {
		return nil, "", ErrInvalidEnrollToken
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()

	var (
		tokenID   string
		expiresAt int64
		usedAt    int64
	)
	err = tx.QueryRow(`SELECT id, expires_at, used_at FROM enroll_tokens WHERE token_hash = ?`,
		hashToken(token)).Scan(&tokenID, &expiresAt, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrInvalidEnrollToken
	}
	if err != nil {
		return nil, "", err
	}
	if usedAt != 0 {
		return nil, "", ErrEnrollTokenUsed
	}
	now := s.now()
	if now.Unix() >= expiresAt {
		return nil, "", ErrEnrollTokenExpired
	}

	// Single-use guard: only one redemption can flip used_at from 0.
	res, err := tx.Exec(`UPDATE enroll_tokens SET used_at = ? WHERE id = ? AND used_at = 0`, now.Unix(), tokenID)
	if err != nil {
		return nil, "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, "", ErrEnrollTokenUsed
	}

	deviceID := "dev_" + randomToken(9)
	deviceToken := randomToken(32)
	if _, err := tx.Exec(
		`INSERT INTO devices (id, name, token_hash, created_at, last_seen) VALUES (?, ?, ?, ?, 0)`,
		deviceID, deviceName, hashToken(deviceToken), now.Unix(),
	); err != nil {
		if isUniqueViolation(err) {
			return nil, "", fmt.Errorf("%w (%s)", ErrDeviceNameTaken, deviceName)
		}
		return nil, "", fmt.Errorf("create device: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return &Device{ID: deviceID, Name: deviceName, CreatedAt: now}, deviceToken, nil
}

// AuthenticateDevice resolves a device token to a non-revoked device.
func (s *Store) AuthenticateDevice(token string) (*Device, error) {
	if token == "" {
		return nil, ErrUnknownDeviceToken
	}
	var (
		d        Device
		created  int64
		lastSeen int64
		revoked  int
	)
	err := s.db.QueryRow(
		`SELECT id, name, created_at, last_seen, revoked FROM devices WHERE token_hash = ?`,
		hashToken(token),
	).Scan(&d.ID, &d.Name, &created, &lastSeen, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownDeviceToken
	}
	if err != nil {
		return nil, err
	}
	if revoked != 0 {
		return nil, ErrDeviceRevoked
	}
	d.CreatedAt = time.Unix(created, 0)
	if lastSeen > 0 {
		d.LastSeen = time.Unix(lastSeen, 0)
	}
	return &d, nil
}

// TouchDevice records that a device was seen.
func (s *Store) TouchDevice(id string) error {
	_, err := s.db.Exec(`UPDATE devices SET last_seen = ? WHERE id = ?`, s.now().Unix(), id)
	return err
}

// ListDevices returns every enrolled device.
func (s *Store) ListDevices() ([]Device, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at, last_seen, revoked FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var (
			d                 Device
			created, lastSeen int64
			revoked           int
		)
		if err := rows.Scan(&d.ID, &d.Name, &created, &lastSeen, &revoked); err != nil {
			return nil, err
		}
		d.CreatedAt = time.Unix(created, 0)
		if lastSeen > 0 {
			d.LastSeen = time.Unix(lastSeen, 0)
		}
		d.Revoked = revoked != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// RevokeDevice permanently disables a device token.
func (s *Store) RevokeDevice(id string) error {
	res, err := s.db.Exec(`UPDATE devices SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDeviceNotFound
	}
	return nil
}
