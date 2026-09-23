// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package connectauth stores revocable browser-approved connections separately
// from the knowledge index. It is a protocol state machine, NOT an identity
// provider: callers must authenticate consent, enforce CSRF and rate limits, and
// recheck the subject's current role, scope and folder ACL on every protected
// request and token renewal. A stored grant never confers administrator status.
package connectauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	ScopeRead         = "read"
	ScopeFull         = "full"
	deviceTTL         = 10 * time.Minute
	accessTTL         = 15 * time.Minute
	grantTTL          = 30 * 24 * time.Hour
	maxDevices        = 128
	maxGrants         = 1024
	maxRefreshHistory = 4096 // exceeds 30 days of quarter-hour renewals; bounds storage
)

var (
	ErrInvalid  = errors.New("invalid_request")
	ErrGrant    = errors.New("invalid_grant")
	ErrPending  = errors.New("authorization_pending")
	ErrSlowDown = errors.New("slow_down")
	ErrDenied   = errors.New("access_denied")
	ErrExpired  = errors.New("expired_token")
	ErrCapacity = errors.New("temporarily_unavailable")
)

// Store uses SQLite transactions to make decisions, redemption, rotation and
// revocation atomic even across two viewer processes. It starts no goroutines.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Device is returned only to the initiating client. DeviceCode is a secret, not
// the code to display. The HTTP adapter adds trusted verification URIs.
type Device struct {
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`
	ExpiresIn  int64  `json:"expires_in"`
	Interval   int64  `json:"interval"`
}

// Request is safe consent metadata; it contains no device or bearer credential.
type Request struct {
	ClientID  string `json:"client_id"`
	Scope     string `json:"scope"`
	Audience  string `json:"audience"`
	ExpiresAt int64  `json:"expires_at"`
}

// Grant identifies the approving subject, not a snapshot of their permissions.
// Subject MUST bind an immutable account generation, not a recyclable row id.
type Grant struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"`
	ClientID  string `json:"client_id"`
	Scope     string `json:"scope"`
	Audience  string `json:"audience"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
}

type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	GrantID      string `json:"grant_id"`
}

// Open requires a dedicated private directory and never opens mesh.db. Existing
// insecure paths are refused, not silently chmodded. URL escaping prevents a
// filename containing '?' or '#' from changing SQLite's connection options.
func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) || filepath.Base(path) != "connections.db" {
		return nil, fmt.Errorf("connection store requires an absolute connections.db path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("connection store directory must be private and not a symlink")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		if err = f.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	for _, name := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		info, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) && name != path {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("connection store files must be private regular files")
		}
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"}, "_txlock": {"immediate"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		_ = db.Close()
		return nil, err
	}
	if version != 0 && version != 1 {
		_ = db.Close()
		return nil, fmt.Errorf("unsupported connection store schema version %d", version)
	}
	_, err = db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS devices (
 device_hash TEXT PRIMARY KEY, user_hash TEXT NOT NULL UNIQUE,
 client_id TEXT NOT NULL, scope TEXT NOT NULL, audience TEXT NOT NULL,
 expires_at INTEGER NOT NULL, next_poll INTEGER NOT NULL DEFAULT 0,
 interval_seconds INTEGER NOT NULL DEFAULT 5,
 state TEXT NOT NULL DEFAULT 'pending', subject TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS grants (
 id TEXT PRIMARY KEY, subject TEXT NOT NULL, client_id TEXT NOT NULL,
 scope TEXT NOT NULL, audience TEXT NOT NULL, created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS tokens (
 hash TEXT PRIMARY KEY, grant_id TEXT NOT NULL REFERENCES grants(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, expires_at INTEGER NOT NULL, used INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS tokens_grant ON tokens(grant_id);
CREATE INDEX IF NOT EXISTS grants_subject ON grants(subject, audience);
PRAGMA user_version=1;`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func secret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func digest(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func userCode(raw string) (string, bool) {
	v := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), "-", ""))
	if len(v) != 8 {
		return "", false
	}
	for _, c := range v {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", c) {
			return "", false
		}
	}
	return v, true
}

func validAudience(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && len(raw) <= 512 &&
		(u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))
}

func validClient(client string) bool {
	return client == "mesh-ide" || client == "mesh-cli" || client == "mesh-mcp"
}
func validScope(scope string) bool { return scope == ScopeRead || scope == ScopeFull }

// Begin creates a bounded, expiring device request. The adapter rate-limits
// unauthenticated callers before calling it. Audience comes from server config.
func (s *Store) Begin(ctx context.Context, client, scope, audience string) (Device, error) {
	if !validClient(client) || !validScope(scope) || !validAudience(audience) {
		return Device{}, ErrInvalid
	}
	code, err := secret("mesh_device_")
	if err != nil {
		return Device{}, err
	}
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return Device{}, err
	}
	user := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM devices WHERE expires_at <= ?", now); err != nil {
		return Device{}, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM devices").Scan(&count); err != nil {
		return Device{}, err
	}
	if count >= maxDevices {
		return Device{}, ErrCapacity
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO devices(device_hash,user_hash,client_id,scope,audience,expires_at) VALUES(?,?,?,?,?,?)", digest(code), digest(user), client, scope, audience, now+int64(deviceTTL/time.Second))
	if err != nil {
		return Device{}, err
	}
	if err = tx.Commit(); err != nil {
		return Device{}, err
	}
	return Device{DeviceCode: code, UserCode: user[:4] + "-" + user[4:], ExpiresIn: int64(deviceTTL / time.Second), Interval: 5}, nil
}

// Inspect is for an authenticated consent page after its rate/CSRF boundary.
// Unknown, expired and already-decided requests are intentionally indistinct.
func (s *Store) Inspect(ctx context.Context, code, audience string) (Request, error) {
	user, ok := userCode(code)
	if !ok {
		return Request{}, ErrGrant
	}
	var r Request
	err := s.db.QueryRowContext(ctx, "SELECT client_id,scope,audience,expires_at FROM devices WHERE user_hash=? AND audience=? AND expires_at>? AND state='pending'", digest(user), audience, s.now().Unix()).Scan(&r.ClientID, &r.Scope, &r.Audience, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrGrant
	}
	return r, err
}

// Cancel lets only the initiating device abandon a pending or approved request.
// It is idempotent and cannot revoke an already-redeemed, independent grant.
func (s *Store) Cancel(ctx context.Context, code, client, audience string) error {
	if !strings.HasPrefix(code, "mesh_device_") || len(code) > 128 || !validClient(client) {
		return ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM devices WHERE device_hash=? AND client_id=? AND audience=?", digest(code), client, audience)
	return err
}

// Decide must only be called after explicit, CSRF-protected browser consent.
// The authenticated subject is supplied by the server, never the request body.
// Consent may narrow full to read, but cannot broaden a read request to full.
func (s *Store) Decide(ctx context.Context, code, subject, audience, scope string, approve bool) error {
	user, ok := userCode(code)
	if !ok || subject == "" || len(subject) > 512 || !validScope(scope) {
		return ErrInvalid
	}
	state := "denied"
	if approve {
		state = "approved"
	}
	r, err := s.db.ExecContext(ctx, `UPDATE devices SET state=?,subject=?,scope=?
WHERE user_hash=? AND audience=? AND expires_at>? AND state='pending' AND (scope=? OR scope='full')`, state, subject, scope, digest(user), audience, s.now().Unix(), scope)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrGrant
	}
	return nil
}

// Poll atomically exchanges one approved request. A repeated/early poll receives
// slow_down and permanently raises this request's interval by five seconds.
func (s *Store) Poll(ctx context.Context, code, client, audience string) (Tokens, error) {
	if !strings.HasPrefix(code, "mesh_device_") || len(code) > 128 || !validClient(client) {
		return Tokens{}, ErrGrant
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback()
	var subject, scope, state string
	var expiry, next, interval int64
	err = tx.QueryRowContext(ctx, "SELECT subject,scope,state,expires_at,next_poll,interval_seconds FROM devices WHERE device_hash=? AND client_id=? AND audience=?", digest(code), client, audience).Scan(&subject, &scope, &state, &expiry, &next, &interval)
	if errors.Is(err, sql.ErrNoRows) {
		return Tokens{}, ErrGrant
	}
	if err != nil {
		return Tokens{}, err
	}
	now := s.now().Unix()
	if expiry <= now {
		return Tokens{}, ErrExpired
	}
	if state == "denied" {
		return Tokens{}, ErrDenied
	}
	result := ErrPending
	if next > now {
		interval += 5
		result = ErrSlowDown
	}
	if result == ErrSlowDown || state == "pending" {
		_, err = tx.ExecContext(ctx, "UPDATE devices SET next_poll=?,interval_seconds=? WHERE device_hash=?", now+interval, interval, digest(code))
		if err != nil {
			return Tokens{}, err
		}
		if err = tx.Commit(); err != nil {
			return Tokens{}, err
		}
		return Tokens{}, result
	}
	if state != "approved" || subject == "" {
		return Tokens{}, ErrGrant
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM grants WHERE expires_at<=? OR revoked=1", now); err != nil {
		return Tokens{}, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM grants").Scan(&count); err != nil {
		return Tokens{}, err
	}
	if count >= maxGrants {
		return Tokens{}, ErrCapacity
	}
	id, err := secret("mesh_grant_")
	if err != nil {
		return Tokens{}, err
	}
	g := Grant{ID: id, Subject: subject, ClientID: client, Scope: scope, Audience: audience, CreatedAt: now, ExpiresAt: now + int64(grantTTL/time.Second)}
	_, err = tx.ExecContext(ctx, "INSERT INTO grants(id,subject,client_id,scope,audience,created_at,expires_at) VALUES(?,?,?,?,?,?,?)", g.ID, g.Subject, g.ClientID, g.Scope, g.Audience, g.CreatedAt, g.ExpiresAt)
	if err != nil {
		return Tokens{}, err
	}
	tokens, err := s.issue(ctx, tx, g)
	if err != nil {
		return Tokens{}, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM devices WHERE device_hash=?", digest(code)); err != nil {
		return Tokens{}, err
	}
	if err = tx.Commit(); err != nil {
		return Tokens{}, err
	}
	return tokens, nil
}

func (s *Store) issue(ctx context.Context, tx *sql.Tx, g Grant) (Tokens, error) {
	access, err := secret("mesh_access_")
	if err != nil {
		return Tokens{}, err
	}
	refresh, err := secret("mesh_refresh_")
	if err != nil {
		return Tokens{}, err
	}
	now := s.now().Unix()
	expiry := min(now+int64(accessTTL/time.Second), g.ExpiresAt)
	if expiry <= now {
		return Tokens{}, ErrGrant
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO tokens(hash,grant_id,kind,expires_at) VALUES(?,?,'access',?),(?,?,'refresh',?)", digest(access), g.ID, expiry, digest(refresh), g.ID, g.ExpiresAt)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", ExpiresIn: expiry - now, Scope: g.Scope, GrantID: g.ID}, nil
}

const grantColumns = "g.id,g.subject,g.client_id,g.scope,g.audience,g.created_at,g.expires_at"

func scanGrant(row *sql.Row) (Grant, error) {
	var g Grant
	err := row.Scan(&g.ID, &g.Subject, &g.ClientID, &g.Scope, &g.Audience, &g.CreatedAt, &g.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Grant{}, ErrGrant
	}
	return g, err
}

// LookupAccess authenticates the connection only. The caller MUST revalidate
// the returned subject's existence/generation and current permissions afterward.
func (s *Store) LookupAccess(ctx context.Context, token, audience string) (Grant, error) {
	if !strings.HasPrefix(token, "mesh_access_") || len(token) > 128 {
		return Grant{}, ErrGrant
	}
	now := s.now().Unix()
	return scanGrant(s.db.QueryRowContext(ctx, "SELECT "+grantColumns+" FROM tokens t JOIN grants g ON g.id=t.grant_id WHERE t.hash=? AND t.kind='access' AND t.expires_at>? AND g.audience=? AND g.expires_at>? AND g.revoked=0", digest(token), now, audience, now))
}

// LookupRefresh lets the HTTP adapter revalidate the approving account before
// rotation, including for consumed tokens (whose replay must still revoke).
func (s *Store) LookupRefresh(ctx context.Context, token, client, audience string) (Grant, error) {
	if !strings.HasPrefix(token, "mesh_refresh_") || len(token) > 128 {
		return Grant{}, ErrGrant
	}
	now := s.now().Unix()
	return scanGrant(s.db.QueryRowContext(ctx, "SELECT "+grantColumns+" FROM tokens t JOIN grants g ON g.id=t.grant_id WHERE t.hash=? AND t.kind='refresh' AND t.expires_at>? AND g.client_id=? AND g.audience=? AND g.expires_at>? AND g.revoked=0", digest(token), now, client, audience, now))
}

// Refresh rotates credentials without expanding scope or extending the absolute
// grant lifetime. Reuse of a consumed refresh token revokes the entire grant,
// including access tokens issued by a racing request. Clients must serialize
// refresh and never blindly retry an uncertain successful response.
func (s *Store) Refresh(ctx context.Context, token, client, audience string) (Tokens, error) {
	if !strings.HasPrefix(token, "mesh_refresh_") || len(token) > 128 {
		return Tokens{}, ErrGrant
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	g, err := scanGrant(tx.QueryRowContext(ctx, "SELECT "+grantColumns+" FROM tokens t JOIN grants g ON g.id=t.grant_id WHERE t.hash=? AND t.kind='refresh' AND t.expires_at>? AND g.client_id=? AND g.audience=? AND g.expires_at>? AND g.revoked=0", digest(token), now, client, audience, now))
	if err != nil {
		return Tokens{}, err
	}
	var used int
	if err = tx.QueryRowContext(ctx, "SELECT used FROM tokens WHERE hash=?", digest(token)).Scan(&used); err != nil {
		return Tokens{}, err
	}
	if used != 0 {
		if _, err = tx.ExecContext(ctx, "UPDATE grants SET revoked=1 WHERE id=?", g.ID); err != nil {
			return Tokens{}, err
		}
		if err = tx.Commit(); err != nil {
			return Tokens{}, err
		}
		return Tokens{}, ErrGrant
	}
	var history int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM tokens WHERE grant_id=? AND kind='refresh'", g.ID).Scan(&history); err != nil {
		return Tokens{}, err
	}
	if history >= maxRefreshHistory {
		return Tokens{}, ErrCapacity
	}
	if _, err = tx.ExecContext(ctx, "UPDATE tokens SET used=1 WHERE hash=?", digest(token)); err != nil {
		return Tokens{}, err
	}
	// Retain consumed refresh hashes for replay detection until grant expiry.
	// Obsolete access tokens are removed so refresh cannot grow those indefinitely.
	if _, err = tx.ExecContext(ctx, "DELETE FROM tokens WHERE grant_id=? AND kind='access'", g.ID); err != nil {
		return Tokens{}, err
	}
	tokens, err := s.issue(ctx, tx, g)
	if err != nil {
		return Tokens{}, err
	}
	if err = tx.Commit(); err != nil {
		return Tokens{}, err
	}
	return tokens, nil
}

// Revoke is account-bound. Possession of a public grant id is not authorization.
func (s *Store) Revoke(ctx context.Context, id, subject, audience string) error {
	r, err := s.db.ExecContext(ctx, "UPDATE grants SET revoked=1 WHERE id=? AND subject=? AND audience=?", id, subject, audience)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrGrant
	}
	return nil
}

// RevokeToken disconnects the presenting client even if its access token has
// expired. Token possession can revoke only that token's own connection. Unknown
// credentials succeed idempotently, without disclosing whether a grant exists.
func (s *Store) RevokeToken(ctx context.Context, token, client, audience string) error {
	if len(token) > 128 || !(strings.HasPrefix(token, "mesh_access_") || strings.HasPrefix(token, "mesh_refresh_")) {
		return ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `UPDATE grants SET revoked=1
WHERE client_id=? AND audience=? AND id IN (SELECT grant_id FROM tokens WHERE hash=?)`, client, audience, digest(token))
	return err
}

// List exposes only this subject's active connections, never token material.
func (s *Store) List(ctx context.Context, subject, audience string) ([]Grant, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+grantColumns+" FROM grants g WHERE subject=? AND audience=? AND expires_at>? AND revoked=0 ORDER BY created_at,id", subject, audience, s.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		var g Grant
		if err = rows.Scan(&g.ID, &g.Subject, &g.ClientID, &g.Scope, &g.Audience, &g.CreatedAt, &g.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
