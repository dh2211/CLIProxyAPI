package auth

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestMarkResult_429WithoutReset_PersistsRateLimitInCDS(t *testing.T) {
	withQuotaCooldownEnabled(t)

	authDir := t.TempDir()
	store := NewFileCooldownStateStore(authDir)
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{
		ID:       "auth-transient-429",
		Provider: "openai",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"gpt-4o": {Status: StatusActive},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register error: %v", errRegister)
	}

	now := time.Now()
	// Transient 429 burst without reset timestamp.
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "openai",
		Model:    "gpt-4o",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Code:       "rate_limit_exceeded",
			Message:    "Rate limit reached for requests per minute",
		},
	})

	records, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load cooldown records error: %v", errLoad)
	}
	if len(records) == 0 {
		t.Fatalf("expected cooldown records, got 0")
	}
	for _, rec := range records {
		if rec.Quota.Exceeded {
			t.Fatalf("expected Quota.Exceeded = false for transient 429 without reset, got true for model %q", rec.Model)
		}
		if rec.Reason != "rate_limit" {
			t.Fatalf("expected Reason = 'rate_limit', got %q for model %q", rec.Reason, rec.Model)
		}
		if rec.Quota.Reason != "rate_limit" {
			t.Fatalf("expected Quota.Reason = 'rate_limit', got %q for model %q", rec.Quota.Reason, rec.Model)
		}
		if rec.NextRetryAfter.IsZero() || rec.NextRetryAfter.Before(now) {
			t.Fatalf("expected NextRetryAfter in near future, got %v for model %q", rec.NextRetryAfter, rec.Model)
		}
		if rec.NextRetryAfter.After(now.Add(15 * time.Second)) {
			t.Fatalf("expected NextRetryAfter near-future within backoff ladder, got %v for model %q (diff %v)",
				rec.NextRetryAfter, rec.Model, rec.NextRetryAfter.Sub(now))
		}
	}

	// Verify in-memory state matches.
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth missing")
	}
	state := updated.ModelStates["gpt-4o"]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Quota.Exceeded {
		t.Fatalf("model state Quota.Exceeded = true, want false")
	}
	if state.Quota.Reason != "rate_limit" {
		t.Fatalf("model state Quota.Reason = %q, want 'rate_limit'", state.Quota.Reason)
	}
	if !state.Unavailable {
		t.Fatalf("model state Unavailable = false, want true during rate limit backoff")
	}
}

func TestMarkResult_UsageLimitReached_MultiHourResetsAt_PersistsQuotaInCDS(t *testing.T) {
	withQuotaCooldownEnabled(t)

	authDir := t.TempDir()
	store := NewFileCooldownStateStore(authDir)
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{
		ID:       "auth-quota-depleted",
		Provider: "codex",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"gpt-5": {Status: StatusActive},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register error: %v", errRegister)
	}

	now := time.Now()
	resetsAtEpoch := now.Add(4 * time.Hour).Unix()
	msg := fmt.Sprintf(`{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","resets_at":%d}}`, resetsAtEpoch)

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    msg,
		},
	})

	records, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load cooldown records error: %v", errLoad)
	}
	if len(records) == 0 {
		t.Fatalf("expected cooldown records, got 0")
	}
	expectedRecovery := time.Unix(resetsAtEpoch, 0)
	for _, rec := range records {
		if !rec.Quota.Exceeded {
			t.Fatalf("expected Quota.Exceeded = true for usage_limit_reached with multi-hour reset, got false for model %q", rec.Model)
		}
		if rec.Reason != "quota" {
			t.Fatalf("expected Reason = 'quota', got %q for model %q", rec.Reason, rec.Model)
		}
		if rec.Quota.Reason != "quota" {
			t.Fatalf("expected Quota.Reason = 'quota', got %q for model %q", rec.Quota.Reason, rec.Model)
		}
		if rec.NextRetryAfter.Before(expectedRecovery.Add(-2*time.Second)) || rec.NextRetryAfter.After(expectedRecovery.Add(2*time.Second)) {
			t.Fatalf("expected NextRetryAfter ~ %v, got %v for model %q", expectedRecovery, rec.NextRetryAfter, rec.Model)
		}
	}
}

func TestMarkResult_429WithResetsInSecondsGreaterThan300_WithoutUsageLimit_PersistsRateLimitInCDS(t *testing.T) {
	withQuotaCooldownEnabled(t)

	authDir := t.TempDir()
	store := NewFileCooldownStateStore(authDir)
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{
		ID:       "auth-long-reset-429",
		Provider: "openai",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"gpt-4o": {Status: StatusActive},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register error: %v", errRegister)
	}

	// 429 with resets_in_seconds > 300 (e.g. 600s = 10 minutes) without usage_limit_reached marker.
	msg := `{"error":{"message":"Too many requests","resets_in_seconds":600}}`
	retryAfter := 600 * time.Second
	now := time.Now()
	manager.MarkResult(context.Background(), Result{
		AuthID:     auth.ID,
		Provider:   "openai",
		Model:      "gpt-4o",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    msg,
		},
	})

	records, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load cooldown records error: %v", errLoad)
	}
	if len(records) == 0 {
		t.Fatalf("expected cooldown records, got 0")
	}
	for _, rec := range records {
		if rec.Quota.Exceeded {
			t.Fatalf("expected Quota.Exceeded = false for 429 with resets_in_seconds > 300 without usage limit, got true for model %q", rec.Model)
		}
		if rec.Reason != "rate_limit" {
			t.Fatalf("expected Reason = 'rate_limit', got %q for model %q", rec.Reason, rec.Model)
		}
		if rec.Quota.Reason != "rate_limit" {
			t.Fatalf("expected Quota.Reason = 'rate_limit', got %q for model %q", rec.Quota.Reason, rec.Model)
		}
		expectedMinRetry := now.Add(590 * time.Second)
		if rec.NextRetryAfter.Before(expectedMinRetry) {
			t.Fatalf("expected NextRetryAfter to preserve reset duration (~600s), got %v", rec.NextRetryAfter)
		}
	}
}

func TestMarkResult_429WithResetsInSecondsUnder300WithoutUsageLimit_PersistsRateLimitInCDS(t *testing.T) {
	withQuotaCooldownEnabled(t)

	authDir := t.TempDir()
	store := NewFileCooldownStateStore(authDir)
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{
		ID:       "auth-short-reset-429",
		Provider: "openai",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"gpt-4o": {Status: StatusActive},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register error: %v", errRegister)
	}

	// 429 with resets_in_seconds <= 300 (e.g. 60s) without usage_limit_reached marker.
	msg := `{"error":{"message":"Rate limit reached, please retry later","resets_in_seconds":60}}`
	retryAfter := 60 * time.Second
	manager.MarkResult(context.Background(), Result{
		AuthID:     auth.ID,
		Provider:   "openai",
		Model:      "gpt-4o",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    msg,
		},
	})

	records, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load cooldown records error: %v", errLoad)
	}
	if len(records) == 0 {
		t.Fatalf("expected cooldown records, got 0")
	}
	for _, rec := range records {
		if rec.Quota.Exceeded {
			t.Fatalf("expected Quota.Exceeded = false for resets_in_seconds <= 300 without usage limit, got true for model %q", rec.Model)
		}
		if rec.Reason != "rate_limit" {
			t.Fatalf("expected Reason = 'rate_limit', got %q for model %q", rec.Reason, rec.Model)
		}
		if rec.Quota.Reason != "rate_limit" {
			t.Fatalf("expected Quota.Reason = 'rate_limit', got %q for model %q", rec.Quota.Reason, rec.Model)
		}
	}
}

func TestMarkResult_429CredentialScope_TransientRateLimit_DoesNotMarkSiblingsAsQuotaExceeded(t *testing.T) {
	withQuotaCooldownEnabled(t)

	authDir := t.TempDir()
	store := NewFileCooldownStateStore(authDir)
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{
		ID:       "auth-credscope-ratelimit",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"claude-3-5-sonnet": {Status: StatusActive},
			"claude-3-5-haiku":  {Status: StatusActive},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register error: %v", errRegister)
	}

	// Credential-scoped transient rate limit (e.g. org RPM burst).
	manager.MarkResult(context.Background(), Result{
		AuthID:          auth.ID,
		Provider:        "claude",
		Model:           "claude-3-5-sonnet",
		Success:         false,
		CredentialScope: true,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Code:       "rate_limit_error",
			Message:    "Number of request tokens has exceeded your per-minute rate limit",
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth missing")
	}

	sonnet := updated.ModelStates["claude-3-5-sonnet"]
	if sonnet == nil {
		t.Fatal("sonnet missing")
	}
	if sonnet.Quota.Exceeded {
		t.Fatalf("sonnet Quota.Exceeded = true, want false")
	}
	if sonnet.Quota.Reason != "rate_limit" {
		t.Fatalf("sonnet Quota.Reason = %q, want 'rate_limit'", sonnet.Quota.Reason)
	}
	if !sonnet.Unavailable {
		t.Fatalf("sonnet Unavailable = false, want true during rate limit backoff")
	}

	haiku := updated.ModelStates["claude-3-5-haiku"]
	if haiku == nil {
		t.Fatal("haiku missing")
	}
	if haiku.Quota.Exceeded {
		t.Fatalf("sibling haiku Quota.Exceeded = true, want false")
	}
	if haiku.Quota.Reason != "rate_limit" {
		t.Fatalf("sibling haiku Quota.Reason = %q, want 'rate_limit'", haiku.Quota.Reason)
	}
	if !haiku.Unavailable {
		t.Fatalf("sibling haiku Unavailable = false, want true during credential-scoped backoff")
	}

	if updated.Quota.Exceeded {
		t.Fatalf("auth Quota.Exceeded = true, want false")
	}

	// Also verify records persisted to .cds store.
	records, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load error: %v", errLoad)
	}
	for _, rec := range records {
		if rec.Quota.Exceeded {
			t.Fatalf("persisted record for model %q has Quota.Exceeded = true, want false", rec.Model)
		}
		if rec.Reason != "rate_limit" {
			t.Fatalf("persisted record for model %q has Reason = %q, want 'rate_limit'", rec.Model, rec.Reason)
		}
	}
}

func TestMarkResult_429PeersServingFailover_Unchanged(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const provider = "openai"
	const model = "gpt-4o"
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})

	auth1 := &Auth{ID: "auth-peer-1", Provider: provider, Status: StatusActive}
	auth2 := &Auth{ID: "auth-peer-2", Provider: provider, Status: StatusActive}

	reg := registry.GetGlobalRegistry()
	models := []*registry.ModelInfo{{ID: model}}
	reg.RegisterClient(auth1.ID, provider, models)
	reg.RegisterClient(auth2.ID, provider, models)
	t.Cleanup(func() {
		reg.UnregisterClient(auth1.ID)
		reg.UnregisterClient(auth2.ID)
	})

	if _, err := manager.Register(WithSkipPersist(context.Background()), auth1); err != nil {
		t.Fatalf("register auth1: %v", err)
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth2); err != nil {
		t.Fatalf("register auth2: %v", err)
	}

	// auth1 encounters transient 429 burst.
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth1.ID,
		Provider: provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Code:       "rate_limit_exceeded",
			Message:    "Rate limit reached",
		},
	})

	// Verify auth1 is cooling but Quota.Exceeded is false.
	updated1, _ := manager.GetByID(auth1.ID)
	state1 := updated1.ModelStates[model]
	if state1 == nil || !state1.Unavailable {
		t.Fatalf("auth1 model state should be unavailable/cooling")
	}
	if state1.Quota.Exceeded {
		t.Fatalf("auth1 Quota.Exceeded should be false for transient rate limit")
	}

	// Selector must pick auth2 because auth1 is cooling.
	picked, errPick := manager.SelectAuth(context.Background(), provider, model, cliproxyexecutor.Options{})
	if errPick != nil || picked == nil {
		t.Fatalf("SelectAuth error: %v", errPick)
	}
	if picked.ID != auth2.ID {
		t.Fatalf("picked auth %q, want peer auth %q", picked.ID, auth2.ID)
	}
}

func TestExistingCDSRecordsStayReadable(t *testing.T) {
	authDir := t.TempDir()
	store := NewFileCooldownStateStore(authDir)

	now := time.Now().UTC().Truncate(time.Second)
	retryTime := now.Add(2 * time.Hour)

	// Simulate an existing .cds record written by an earlier version with Quota.Exceeded = true.
	existingRecord := CooldownStateRecord{
		Provider:       "codex",
		AuthID:         "legacy-auth-1",
		Model:          "gpt-5",
		Status:         "cooling",
		NextRetryAfter: retryTime,
		Reason:         "quota",
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: retryTime,
			BackoffLevel:  2,
		},
		UpdatedAt: now,
	}

	if errSave := store.Save(context.Background(), []CooldownStateRecord{existingRecord}); errSave != nil {
		t.Fatalf("Save error: %v", errSave)
	}

	// Verify the file exists on disk.
	cdsPath := filepath.Join(authDir, "legacy-auth-1.cds")
	if _, errStat := os.Stat(cdsPath); errStat != nil {
		t.Fatalf(".cds file not found on disk: %v", errStat)
	}

	// Manager restores from store.
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{ID: "legacy-auth-1", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register error: %v", errRegister)
	}

	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates error: %v", errRestore)
	}

	restoredAuth, ok := manager.GetByID("legacy-auth-1")
	if !ok || restoredAuth == nil {
		t.Fatal("auth missing")
	}
	state := restoredAuth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("restored model state missing")
	}
	if !state.Quota.Exceeded {
		t.Fatalf("expected restored state Quota.Exceeded = true")
	}
	if state.Quota.Reason != "quota" {
		t.Fatalf("expected restored state Quota.Reason = 'quota', got %q", state.Quota.Reason)
	}
	if state.Quota.BackoffLevel != 2 {
		t.Fatalf("expected restored state BackoffLevel = 2, got %d", state.Quota.BackoffLevel)
	}
}

func TestMarkResult_CooldownMonotonicity_PreservesLongerDeadline(t *testing.T) {
	withQuotaCooldownEnabled(t)

	for _, tc := range []struct {
		name          string
		initialQuota  bool
		initialReason string
	}{
		{name: "active_rate_limit", initialQuota: false, initialReason: "rate_limit"},
		{name: "active_quota", initialQuota: true, initialReason: "quota"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := &Auth{
				ID:       "auth-monotonic-" + tc.name,
				Provider: "openai",
				Status:   StatusActive,
				ModelStates: map[string]*ModelState{
					"model-a": {Status: StatusActive},
					"model-b": {Status: StatusActive},
				},
			}
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
				t.Fatalf("Register error: %v", errRegister)
			}

			// Establish initial long cooldown (1 hour).
			initialDuration := 1 * time.Hour
			initialErr := &Error{
				HTTPStatus: http.StatusTooManyRequests,
				Message:    "initial failure",
			}
			if tc.initialQuota {
				initialErr.Code = "usage_limit_reached"
			}
			manager.MarkResult(context.Background(), Result{
				AuthID:          auth.ID,
				Provider:        "openai",
				Model:           "model-a",
				Success:         false,
				CredentialScope: true,
				RetryAfter:      &initialDuration,
				Error:           initialErr,
			})

			beforeShort, ok := manager.GetByID(auth.ID)
			if !ok || beforeShort == nil {
				t.Fatal("auth missing")
			}
			modelBefore := beforeShort.ModelStates["model-a"].Quota.NextRecoverAt
			siblingBefore := beforeShort.ModelStates["model-b"].Quota.NextRecoverAt
			authBefore := beforeShort.Quota.NextRecoverAt

			if modelBefore.IsZero() || siblingBefore.IsZero() || authBefore.IsZero() {
				t.Fatalf("expected initial deadlines to be set: model=%v sibling=%v auth=%v",
					modelBefore, siblingBefore, authBefore)
			}

			// Later shorter 429 arrives (5 seconds reset).
			shorterDuration := 5 * time.Second
			manager.MarkResult(context.Background(), Result{
				AuthID:          auth.ID,
				Provider:        "openai",
				Model:           "model-a",
				Success:         false,
				CredentialScope: true,
				RetryAfter:      &shorterDuration,
				Error: &Error{
					HTTPStatus: http.StatusTooManyRequests,
					Message:    "later shorter rate limit",
				},
			})

			afterShort, ok := manager.GetByID(auth.ID)
			if !ok || afterShort == nil {
				t.Fatal("auth missing")
			}
			modelAfter := afterShort.ModelStates["model-a"].Quota.NextRecoverAt
			siblingAfter := afterShort.ModelStates["model-b"].Quota.NextRecoverAt
			authAfter := afterShort.Quota.NextRecoverAt

			if !modelAfter.Equal(modelBefore) {
				t.Fatalf("model scope NextRecoverAt shortened or changed: got %v, want %v", modelAfter, modelBefore)
			}
			if !siblingAfter.Equal(siblingBefore) {
				t.Fatalf("sibling scope NextRecoverAt shortened or changed: got %v, want %v", siblingAfter, siblingBefore)
			}
			if !authAfter.Equal(authBefore) {
				t.Fatalf("auth scope NextRecoverAt shortened or changed: got %v, want %v", authAfter, authBefore)
			}

			// Also verify direct applyAuthFailureState monotonicity
			authDirect := &Auth{
				ID:       "auth-direct-" + tc.name,
				Provider: "openai",
				Status:   StatusActive,
			}
			now := time.Now()
			applyAuthFailureState(authDirect, initialErr, &initialDuration, now, false)
			directBefore := authDirect.Quota.NextRecoverAt
			applyAuthFailureState(authDirect, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "short failure"}, &shorterDuration, now, false)
			directAfter := authDirect.Quota.NextRecoverAt
			if !directAfter.Equal(directBefore) {
				t.Fatalf("applyAuthFailureState NextRecoverAt shortened: got %v, want %v", directAfter, directBefore)
			}
		})
	}
}

func TestMarkResult_GeminiCheckQuota_DoesNotMarkQuotaExceeded(t *testing.T) {
	withQuotaCooldownEnabled(t)

	authDir := t.TempDir()
	store := NewFileCooldownStateStore(authDir)
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{
		ID:       "auth-gemini-check-quota",
		Provider: "gemini",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"gemini-2.5-flash": {Status: StatusActive},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register error: %v", errRegister)
	}

	// Upstream Gemini 429 rate limit response:
	// "Resource has been exhausted (e.g. check quota)"
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "gemini",
		Model:    "gemini-2.5-flash",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Code:       "RESOURCE_EXHAUSTED",
			Message:    "Resource has been exhausted (e.g. check quota)",
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth missing")
	}
	state := updated.ModelStates["gemini-2.5-flash"]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Quota.Exceeded {
		t.Fatalf("expected Quota.Exceeded = false for Gemini 'check quota' transient 429, got true")
	}
	if state.Quota.Reason != "rate_limit" {
		t.Fatalf("expected Quota.Reason = 'rate_limit', got %q", state.Quota.Reason)
	}
	if updated.Quota.Exceeded {
		t.Fatalf("expected auth Quota.Exceeded = false, got true")
	}

	// Verify persisted CDS records do not record quota exceeded.
	records, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load error: %v", errLoad)
	}
	for _, rec := range records {
		if rec.Quota.Exceeded {
			t.Fatalf("persisted record has Quota.Exceeded = true, want false for Gemini rate limit")
		}
		if rec.Reason != "rate_limit" {
			t.Fatalf("persisted record has Reason = %q, want 'rate_limit'", rec.Reason)
		}
	}
}

func TestMarkResult_429WithResetGreaterThan30MinWithoutUsageLimit_DoesNotMarkQuotaExceeded(t *testing.T) {
	withQuotaCooldownEnabled(t)

	authDir := t.TempDir()
	store := NewFileCooldownStateStore(authDir)
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{
		ID:       "auth-429-reset-30min",
		Provider: "openai",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"gpt-4o": {Status: StatusActive},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register error: %v", errRegister)
	}

	// 429 with reset duration >= 30min (e.g. 45 minutes = 2700s) without usage limit signal.
	retryAfter := 45 * time.Minute
	now := time.Now()
	manager.MarkResult(context.Background(), Result{
		AuthID:     auth.ID,
		Provider:   "openai",
		Model:      "gpt-4o",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Code:       "rate_limit_exceeded",
			Message:    "Requests rate limit exceeded, please retry after 45m",
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth missing")
	}
	state := updated.ModelStates["gpt-4o"]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Quota.Exceeded {
		t.Fatalf("expected Quota.Exceeded = false for 429 with reset >= 30m without usage limit signal, got true")
	}
	if state.Quota.Reason != "rate_limit" {
		t.Fatalf("expected Quota.Reason = 'rate_limit', got %q", state.Quota.Reason)
	}
	expectedMinNext := now.Add(44 * time.Minute)
	if state.Quota.NextRecoverAt.Before(expectedMinNext) {
		t.Fatalf("expected NextRecoverAt to reflect ~45m reset, got %v", state.Quota.NextRecoverAt)
	}

	records, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load error: %v", errLoad)
	}
	for _, rec := range records {
		if rec.Quota.Exceeded {
			t.Fatalf("persisted record has Quota.Exceeded = true, want false")
		}
		if rec.Reason != "rate_limit" {
			t.Fatalf("persisted record has Reason = %q, want 'rate_limit'", rec.Reason)
		}
	}
}

func TestMarkResult_ModelScopedRateLimit_DoesNotMarkCredentialUnavailableOrBlockSiblings(t *testing.T) {
	withQuotaCooldownEnabled(t)

	authDir := t.TempDir()
	store := NewFileCooldownStateStore(authDir)
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)

	auth := &Auth{
		ID:       "auth-model-ratelimit",
		Provider: "openai",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"gpt-4o":      {Status: StatusActive},
			"gpt-4o-mini": {Status: StatusActive},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register error: %v", errRegister)
	}

	retry := 30 * time.Second
	manager.MarkResult(context.Background(), Result{
		AuthID:          auth.ID,
		Provider:        "openai",
		Model:           "gpt-4o",
		Success:         false,
		RetryAfter:      &retry,
		CredentialScope: false,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Code:       "rate_limit_exceeded",
			Message:    "Rate limit reached for model gpt-4o",
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth missing")
	}

	m1 := updated.ModelStates["gpt-4o"]
	if m1 == nil {
		t.Fatal("gpt-4o missing")
	}
	if !m1.Unavailable {
		t.Fatal("gpt-4o should be unavailable during rate limit cooldown")
	}
	if m1.Quota.Exceeded {
		t.Fatal("gpt-4o Quota.Exceeded should be false")
	}
	if m1.Quota.Reason != "rate_limit" {
		t.Fatalf("gpt-4o Quota.Reason = %q, want 'rate_limit'", m1.Quota.Reason)
	}

	m2 := updated.ModelStates["gpt-4o-mini"]
	if m2 == nil {
		t.Fatal("gpt-4o-mini missing")
	}
	if m2.Unavailable {
		t.Fatal("sibling gpt-4o-mini should NOT be marked unavailable")
	}
	if m2.Quota.Exceeded || m2.Quota.Reason != "" {
		t.Fatalf("sibling gpt-4o-mini should not have quota failure, got reason=%q exceeded=%v", m2.Quota.Reason, m2.Quota.Exceeded)
	}

	if updated.Unavailable {
		t.Fatal("credential must NOT be marked unavailable when healthy sibling models exist")
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:            auth.ID,
		Provider:          "openai",
		Model:             "gpt-4o-mini",
		Success:           true,
		CompletedResponse: true,
	})

	updated, _ = manager.GetByID(auth.ID)
	if !updated.InferenceVerified {
		t.Fatal("successful response on healthy sibling must set InferenceVerified = true")
	}
	if updated.Unavailable {
		t.Fatal("auth should remain available after sibling success")
	}
	m1 = updated.ModelStates["gpt-4o"]
	if !m1.Unavailable || m1.Quota.Reason != "rate_limit" {
		t.Fatal("gpt-4o should retain its rate limit cooldown after sibling success")
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:          auth.ID,
		Provider:        "openai",
		Model:           "gpt-4o-mini",
		Success:         false,
		CredentialScope: false,
		Error: &Error{
			HTTPStatus: http.StatusInternalServerError,
			Message:    "internal error",
		},
	})

	updated, _ = manager.GetByID(auth.ID)
	m2 = updated.ModelStates["gpt-4o-mini"]
	if m2.Quota.Reason == "credential_quota" {
		t.Fatalf("sibling model must not receive credential_quota wrap on non-credential failure, got %q", m2.Quota.Reason)
	}

	records, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load error: %v", errLoad)
	}
	for _, rec := range records {
		if rec.Model == "gpt-4o" {
			if rec.Reason != "rate_limit" {
				t.Fatalf("persisted record for gpt-4o has reason %q, want 'rate_limit'", rec.Reason)
			}
			if rec.Quota.Reason != "rate_limit" {
				t.Fatalf("persisted record Quota.Reason has %q, want 'rate_limit'", rec.Quota.Reason)
			}
			if rec.Quota.Exceeded {
				t.Fatal("persisted record Quota.Exceeded should be false")
			}
		}
	}
}
