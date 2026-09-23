// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package connectauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAudience = "https://mesh.example.test/app"
const testSubject = "member:17:immutable-generation"

func testStore(t *testing.T) (*Store, string, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private ?# state", "connections.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Unix(1_790_000_000, 0)
	s.now = func() time.Time { return now }
	return s, path, &now
}

func approved(t *testing.T, s *Store, scope string) (Device, Tokens) {
	t.Helper()
	ctx := context.Background()
	d, err := s.Begin(ctx, "mesh-ide", scope, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Decide(ctx, d.UserCode, testSubject, testAudience, scope, true); err != nil {
		t.Fatal(err)
	}
	tokens, err := s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience)
	if err != nil {
		t.Fatal(err)
	}
	return d, tokens
}

func TestApprovalLifecycleAndAudienceBinding(t *testing.T) {
	s, _, now := testStore(t)
	ctx := context.Background()
	d, err := s.Begin(ctx, "mesh-ide", ScopeFull, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	if d.ExpiresIn != 600 || d.Interval != 5 || len(d.UserCode) != 9 || strings.Contains(d.DeviceCode, d.UserCode) {
		t.Fatal("incorrect device response")
	}
	for _, audience := range []string{"https://other.example.test/app", "https://mesh.example.test/other"} {
		if _, err = s.Inspect(ctx, d.UserCode, audience); !errors.Is(err, ErrGrant) {
			t.Fatalf("inspect audience: %v", err)
		}
		if _, err = s.Poll(ctx, d.DeviceCode, "mesh-ide", audience); !errors.Is(err, ErrGrant) {
			t.Fatalf("poll audience: %v", err)
		}
		if err = s.Decide(ctx, d.UserCode, testSubject, audience, ScopeFull, true); !errors.Is(err, ErrGrant) {
			t.Fatalf("decide audience: %v", err)
		}
	}
	r, err := s.Inspect(ctx, strings.ToLower(d.UserCode), testAudience)
	if err != nil || r.Scope != ScopeFull || r.ClientID != "mesh-ide" {
		t.Fatalf("inspect: %+v %v", r, err)
	}
	if _, err = s.Poll(ctx, d.DeviceCode, "mesh-cli", testAudience); !errors.Is(err, ErrGrant) {
		t.Fatalf("wrong client: %v", err)
	}
	if _, err = s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience); !errors.Is(err, ErrPending) {
		t.Fatalf("pending: %v", err)
	}
	if err = s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeRead, true); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(5 * time.Second)
	tokens, err := s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience)
	if err != nil || tokens.Scope != ScopeRead || tokens.ExpiresIn != 900 || tokens.TokenType != "Bearer" {
		t.Fatalf("redeem: %+v %v", tokens, err)
	}
	if _, err = s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience); !errors.Is(err, ErrGrant) {
		t.Fatalf("second redemption: %v", err)
	}
	if err = s.Decide(ctx, d.UserCode, "another-member", testAudience, ScopeRead, true); !errors.Is(err, ErrGrant) {
		t.Fatalf("second decision: %v", err)
	}
	g, err := s.LookupAccess(ctx, tokens.AccessToken, testAudience)
	if err != nil || g.Subject != testSubject || g.Scope != ScopeRead || g.ID != tokens.GrantID {
		t.Fatalf("lookup: %+v %v", g, err)
	}
	if _, err = s.LookupAccess(ctx, tokens.AccessToken, "https://other.example.test/app"); !errors.Is(err, ErrGrant) {
		t.Fatalf("wrong token audience: %v", err)
	}
	if _, err = s.LookupAccess(ctx, tokens.RefreshToken, testAudience); !errors.Is(err, ErrGrant) {
		t.Fatalf("refresh used as access: %v", err)
	}
}

func TestPollingBackoffDenialAndExpiry(t *testing.T) {
	s, _, now := testStore(t)
	ctx := context.Background()
	d, _ := s.Begin(ctx, "mesh-cli", ScopeRead, testAudience)
	if _, err := s.Poll(ctx, d.DeviceCode, "mesh-cli", testAudience); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	if _, err := s.Poll(ctx, d.DeviceCode, "mesh-cli", testAudience); !errors.Is(err, ErrSlowDown) {
		t.Fatal(err)
	}
	*now = now.Add(5 * time.Second)
	if _, err := s.Poll(ctx, d.DeviceCode, "mesh-cli", testAudience); !errors.Is(err, ErrSlowDown) {
		t.Fatalf("backoff was not retained: %v", err)
	}
	*now = now.Add(15 * time.Second)
	if _, err := s.Poll(ctx, d.DeviceCode, "mesh-cli", testAudience); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeRead, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Poll(ctx, d.DeviceCode, "mesh-cli", testAudience); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeRead, true); !errors.Is(err, ErrGrant) {
		t.Fatalf("denial overturned: %v", err)
	}
	d, _ = s.Begin(ctx, "mesh-cli", ScopeRead, testAudience)
	*now = now.Add(deviceTTL)
	if _, err := s.Poll(ctx, d.DeviceCode, "mesh-cli", testAudience); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiry: %v", err)
	}
	if _, err := s.Inspect(ctx, d.UserCode, testAudience); !errors.Is(err, ErrGrant) {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeRead, true); !errors.Is(err, ErrGrant) {
		t.Fatal(err)
	}
}

func TestScopeCannotExpandAndPrincipalCannotBeReplaced(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	d, _ := s.Begin(ctx, "mesh-ide", ScopeRead, testAudience)
	if err := s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeFull, true); !errors.Is(err, ErrGrant) {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, d.UserCode, "", testAudience, ScopeRead, true); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeRead, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, d.UserCode, "attacker", testAudience, ScopeRead, true); !errors.Is(err, ErrGrant) {
		t.Fatal(err)
	}
	tokens, err := s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.LookupAccess(ctx, tokens.AccessToken, testAudience)
	if err != nil || g.Subject != testSubject {
		t.Fatalf("subject changed: %+v %v", g, err)
	}
}

func TestRefreshRotationReplayRevokesEntireGrant(t *testing.T) {
	s, _, now := testStore(t)
	ctx := context.Background()
	_, original := approved(t, s, ScopeFull)
	if _, err := s.Refresh(ctx, original.RefreshToken, "mesh-cli", testAudience); !errors.Is(err, ErrGrant) {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, original.RefreshToken, "mesh-ide", "https://other.example.test"); !errors.Is(err, ErrGrant) {
		t.Fatal(err)
	}
	*now = now.Add(accessTTL)
	if _, err := s.LookupAccess(ctx, original.AccessToken, testAudience); !errors.Is(err, ErrGrant) {
		t.Fatalf("expired access: %v", err)
	}
	next, err := s.Refresh(ctx, original.RefreshToken, "mesh-ide", testAudience)
	if err != nil || next.Scope != ScopeFull || next.GrantID != original.GrantID || next.RefreshToken == original.RefreshToken {
		t.Fatalf("rotation failed: %v", err)
	}
	if _, err = s.LookupAccess(ctx, next.AccessToken, testAudience); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, original.RefreshToken, "mesh-ide", testAudience); !errors.Is(err, ErrGrant) {
		t.Fatalf("replay accepted: %v", err)
	}
	if _, err = s.LookupAccess(ctx, next.AccessToken, testAudience); !errors.Is(err, ErrGrant) {
		t.Fatalf("replay did not revoke access: %v", err)
	}
	if _, err = s.Refresh(ctx, next.RefreshToken, "mesh-ide", testAudience); !errors.Is(err, ErrGrant) {
		t.Fatalf("replay did not revoke refresh: %v", err)
	}
}

func TestGrantLifetimeIsAbsoluteAndAccessIsClamped(t *testing.T) {
	s, _, now := testStore(t)
	ctx := context.Background()
	_, tokens := approved(t, s, ScopeRead)
	*now = now.Add(grantTTL - time.Minute)
	last, err := s.Refresh(ctx, tokens.RefreshToken, "mesh-ide", testAudience)
	if err != nil || last.ExpiresIn != 60 {
		t.Fatalf("unbounded lifetime: %d %v", last.ExpiresIn, err)
	}
	*now = now.Add(time.Minute)
	if _, err = s.LookupAccess(ctx, last.AccessToken, testAudience); !errors.Is(err, ErrGrant) {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, last.RefreshToken, "mesh-ide", testAudience); !errors.Is(err, ErrGrant) {
		t.Fatal(err)
	}
}

func TestDurableRevocationAndAccountIsolation(t *testing.T) {
	s, path, now := testStore(t)
	ctx := context.Background()
	_, tokens := approved(t, s, ScopeFull)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return *now }
	if _, err = s.LookupAccess(ctx, tokens.AccessToken, testAudience); err != nil {
		t.Fatalf("restart lost grant: %v", err)
	}
	if list, err := s.List(ctx, "other-subject", testAudience); err != nil || len(list) != 0 {
		t.Fatalf("cross-account list: %+v %v", list, err)
	}
	if err = s.Revoke(ctx, tokens.GrantID, "other-subject", testAudience); !errors.Is(err, ErrGrant) {
		t.Fatalf("cross-account revoke: %v", err)
	}
	if err = s.Revoke(ctx, tokens.GrantID, testSubject, "https://other.example.test"); !errors.Is(err, ErrGrant) {
		t.Fatal(err)
	}
	list, err := s.List(ctx, testSubject, testAudience)
	if err != nil || len(list) != 1 || list[0].ID != tokens.GrantID {
		t.Fatalf("own list: %+v %v", list, err)
	}
	if err = s.Revoke(ctx, tokens.GrantID, testSubject, testAudience); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return *now }
	if _, err = s.LookupAccess(ctx, tokens.AccessToken, testAudience); !errors.Is(err, ErrGrant) {
		t.Fatalf("restart revived grant: %v", err)
	}
	if _, err = s.Refresh(ctx, tokens.RefreshToken, "mesh-ide", testAudience); !errors.Is(err, ErrGrant) {
		t.Fatal(err)
	}
}

func TestConcurrentRedemptionAcrossStores(t *testing.T) {
	s, path, now := testStore(t)
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.now = func() time.Time { return *now }
	ctx := context.Background()
	d, err := s.Begin(ctx, "mesh-ide", ScopeRead, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeRead, true); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		worker := s
		if i%2 == 1 {
			worker = other
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := worker.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrGrant) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("single-use redemption succeeded %d times", successes)
	}
}

func TestConcurrentRefreshReplayFailsClosed(t *testing.T) {
	s, path, now := testStore(t)
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.now = func() time.Time { return *now }
	ctx := context.Background()
	_, original := approved(t, s, ScopeFull)
	type result struct {
		tokens Tokens
		err    error
	}
	results := make(chan result, 2)
	for _, worker := range []*Store{s, other} {
		go func() {
			v, err := worker.Refresh(ctx, original.RefreshToken, "mesh-ide", testAudience)
			results <- result{v, err}
		}()
	}
	a, b := <-results, <-results
	if a.err != nil {
		a, b = b, a
	}
	if a.err != nil || !errors.Is(b.err, ErrGrant) {
		t.Fatalf("rotation not single-use: %v %v", a.err, b.err)
	}
	if _, err = s.LookupAccess(ctx, a.tokens.AccessToken, testAudience); !errors.Is(err, ErrGrant) {
		t.Fatalf("concurrent replay failed open: %v", err)
	}
}

func TestStoreContainsOnlyCredentialHashes(t *testing.T) {
	s, path, _ := testStore(t)
	ctx := context.Background()
	d, tokens := approved(t, s, ScopeRead)
	// Include a still-pending code as well as issued credentials.
	pending, err := s.Begin(ctx, "mesh-cli", ScopeRead, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	// Inspect while open as well: raw credentials must not appear in the WAL.
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		data, err := os.ReadFile(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{d.DeviceCode, tokens.AccessToken, tokens.RefreshToken, pending.DeviceCode, strings.ReplaceAll(pending.UserCode, "-", "")} {
			if strings.Contains(string(data), raw) {
				t.Fatal("plaintext credential persisted")
			}
		}
		info, _ := os.Stat(name)
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("insecure mode: %v", info.Mode())
		}
	}
}

func TestCancellationAndSelfRevocation(t *testing.T) {
	s, _, now := testStore(t)
	ctx := context.Background()
	d, err := s.Begin(ctx, "mesh-ide", ScopeFull, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Cancel(ctx, d.DeviceCode, "mesh-cli", testAudience); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Inspect(ctx, d.UserCode, testAudience); err != nil {
		t.Fatal("another client cancelled the request")
	}
	if err = s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeFull, true); err != nil {
		t.Fatal(err)
	}
	if err = s.Cancel(ctx, d.DeviceCode, "mesh-ide", testAudience); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience); !errors.Is(err, ErrGrant) {
		t.Fatal("cancelled request redeemed")
	}
	if err = s.Cancel(ctx, d.DeviceCode, "mesh-ide", testAudience); err != nil {
		t.Fatal("cancel not idempotent")
	}
	d, tokens := approved(t, s, ScopeFull)
	if err = s.Cancel(ctx, d.DeviceCode, "mesh-ide", testAudience); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LookupAccess(ctx, tokens.AccessToken, testAudience); err != nil {
		t.Fatal("cancel revoked independent grant")
	}
	if err = s.RevokeToken(ctx, tokens.RefreshToken, "mesh-cli", testAudience); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeToken(ctx, tokens.RefreshToken, "mesh-ide", "https://other.example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LookupAccess(ctx, tokens.AccessToken, testAudience); err != nil {
		t.Fatal("cross-client or cross-audience revoke succeeded")
	}
	*now = now.Add(accessTTL)
	if err = s.RevokeToken(ctx, tokens.RefreshToken, "mesh-ide", testAudience); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, tokens.RefreshToken, "mesh-ide", testAudience); !errors.Is(err, ErrGrant) {
		t.Fatal("self-revoked grant renewed")
	}
	if err = s.RevokeToken(ctx, tokens.RefreshToken, "mesh-ide", testAudience); err != nil {
		t.Fatal("revoke not idempotent")
	}
}

func TestValidationCapacityAndCancellation(t *testing.T) {
	s, _, now := testStore(t)
	ctx := context.Background()
	for _, args := range [][3]string{{"unknown", ScopeFull, testAudience}, {"mesh-ide", "admin", testAudience}, {"mesh-ide", ScopeRead, "http://example.test"}, {"mesh-ide", ScopeRead, "https://user:pass@example.test"}, {"mesh-ide", ScopeRead, "https://example.test?token=bad"}} {
		if _, err := s.Begin(ctx, args[0], args[1], args[2]); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad input accepted: %v", err)
		}
	}
	for i := 0; i < maxDevices; i++ {
		if _, err := s.Begin(ctx, "mesh-ide", ScopeRead, testAudience); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Begin(ctx, "mesh-ide", ScopeRead, testAudience); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unbounded requests: %v", err)
	}
	*now = now.Add(deviceTTL)
	if _, err := s.Begin(ctx, "mesh-ide", ScopeRead, testAudience); err != nil {
		t.Fatalf("expired slots not reclaimed: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Begin(cancelled, "mesh-ide", ScopeRead, testAudience); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
}

func TestOpenRejectsInsecurePathsAndFutureSchema(t *testing.T) {
	if _, err := Open("connections.db"); err == nil {
		t.Fatal("relative path accepted")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "mesh.db")); err == nil {
		t.Fatal("index path accepted")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(dir, "connections.db")); err == nil {
		t.Fatal("public directory accepted")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "connections.db")
	if err := os.Symlink(filepath.Join(dir, "other"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("symlink accepted")
	}
	s, path, _ := testStore(t)
	if _, err := s.db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("future schema accepted")
	}
}

func TestIssuanceAndRotationRollBackOnStorageFailure(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	d, err := s.Begin(ctx, "mesh-ide", ScopeFull, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeFull, true); err != nil {
		t.Fatal(err)
	}
	fail := func() {
		t.Helper()
		_, err := s.db.Exec("CREATE TRIGGER reject_refresh BEFORE INSERT ON tokens WHEN NEW.kind='refresh' BEGIN SELECT RAISE(ABORT,'test storage failure'); END")
		if err != nil {
			t.Fatal(err)
		}
	}
	recoverStore := func() {
		t.Helper()
		if _, err := s.db.Exec("DROP TRIGGER reject_refresh"); err != nil {
			t.Fatal(err)
		}
	}
	fail()
	if tokens, err := s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience); err == nil || tokens.AccessToken != "" {
		t.Fatal("failed issuance returned a credential")
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM grants").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial grant committed: %d %v", count, err)
	}
	recoverStore()
	tokens, err := s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience)
	if err != nil {
		t.Fatalf("failed issuance consumed approval: %v", err)
	}
	fail()
	if next, err := s.Refresh(ctx, tokens.RefreshToken, "mesh-ide", testAudience); err == nil || next.AccessToken != "" {
		t.Fatal("failed rotation returned a credential")
	}
	if _, err = s.LookupAccess(ctx, tokens.AccessToken, testAudience); err != nil {
		t.Fatalf("failed rotation removed existing access: %v", err)
	}
	recoverStore()
	if _, err = s.Refresh(ctx, tokens.RefreshToken, "mesh-ide", testAudience); err != nil {
		t.Fatalf("failed rotation consumed refresh token: %v", err)
	}
}

func TestApprovalRaceChoosesExactlyOneSubject(t *testing.T) {
	s, path, now := testStore(t)
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.now = func() time.Time { return *now }
	ctx := context.Background()
	d, err := s.Begin(ctx, "mesh-ide", ScopeFull, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	type decision struct {
		subject string
		err     error
	}
	results := make(chan decision, 2)
	for i, worker := range []*Store{s, other} {
		subject := []string{"first-member", "second-member"}[i]
		go func() {
			results <- decision{subject, worker.Decide(ctx, d.UserCode, subject, testAudience, ScopeFull, true)}
		}()
	}
	a, b := <-results, <-results
	if a.err != nil {
		a, b = b, a
	}
	if a.err != nil || !errors.Is(b.err, ErrGrant) {
		t.Fatalf("decision not single-use: %v %v", a.err, b.err)
	}
	tokens, err := s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.LookupAccess(ctx, tokens.AccessToken, testAudience)
	if err != nil || g.Subject != a.subject {
		t.Fatalf("grant does not match winning consent: %+v %v", g, err)
	}
}

func TestPendingAndApprovedRequestsSurviveRestart(t *testing.T) {
	s, path, now := testStore(t)
	ctx := context.Background()
	d, err := s.Begin(ctx, "mesh-ide", ScopeFull, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	for _, approved := range []bool{false, true} {
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		s.now = func() time.Time { return *now }
		if !approved {
			if _, err = s.Inspect(ctx, d.UserCode, testAudience); err != nil {
				t.Fatalf("restart lost pending request: %v", err)
			}
			if err = s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeFull, true); err != nil {
				t.Fatal(err)
			}
		} else if _, err = s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience); err != nil {
			t.Fatalf("restart lost approved request: %v", err)
		}
	}
}

func TestGrantAndRefreshHistoryCapacity(t *testing.T) {
	s, _, now := testStore(t)
	ctx := context.Background()
	_, tokens := approved(t, s, ScopeFull)
	// Fill using SQL to keep the limit test independent of randomness and fast.
	_, err := s.db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO tokens(hash,grant_id,kind,expires_at,used) SELECT 'spent-'||x,?,'refresh',?,1 FROM n`, maxRefreshHistory-1, tokens.GrantID, now.Add(grantTTL).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, tokens.RefreshToken, "mesh-ide", testAudience); !errors.Is(err, ErrCapacity) {
		t.Fatalf("refresh history unbounded: %v", err)
	}
	if _, err = s.LookupAccess(ctx, tokens.AccessToken, testAudience); err != nil {
		t.Fatalf("capacity failure broke current access: %v", err)
	}
	_, err = s.db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO grants(id,subject,client_id,scope,audience,created_at,expires_at) SELECT 'filler-'||x,?,'mesh-ide','read',?,?,? FROM n`, maxGrants-1, testSubject, testAudience, now.Unix(), now.Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Begin(ctx, "mesh-ide", ScopeRead, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeRead, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience); !errors.Is(err, ErrCapacity) {
		t.Fatalf("grant storage unbounded: %v", err)
	}
	*now = now.Add(2 * time.Hour)
	d, err = s.Begin(ctx, "mesh-ide", ScopeRead, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Decide(ctx, d.UserCode, testSubject, testAudience, ScopeRead, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Poll(ctx, d.DeviceCode, "mesh-ide", testAudience); err != nil {
		t.Fatalf("expired grants not reclaimed: %v", err)
	}
}
