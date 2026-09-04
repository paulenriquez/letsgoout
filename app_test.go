package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

const testOrigin = "https://letsgoout.test"

func testApp(t *testing.T, now *time.Time) *app {
	t.Helper()
	db, err := openDatabase(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	cfg := config{
		PublicBaseURL: testOrigin, DatabasePath: "unused", RateLimitHMACKey: []byte("0123456789abcdef0123456789abcdef"),
		CreateHourlyLimit: 5, CreateDailyLimit: 20, GlobalDailyLimit: 500, ViewMinuteLimit: 1000, AcceptMinuteLimit: 1000,
	}
	return newApp(db, cfg, embeddedFiles, func() time.Time { return *now }, rand.Reader)
}

func request(t *testing.T, handler http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.8:4321"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
}

func validCreate(now time.Time) createRequest {
	return createRequest{
		AskerName:     "Alex",
		RecipientName: "Taylor",
		OfferedIdeas:  []string{"pizza", "coffee"},
		ProposedSlots: []string{now.Add(2 * time.Hour).Format(time.RFC3339), now.Add(25 * time.Hour).Format(time.RFC3339)},
	}
}

func createTokens(t *testing.T, handler http.Handler, now time.Time, input createRequest) (string, string, time.Time) {
	t.Helper()
	response := request(t, handler, http.MethodPost, "/api/invites", input)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		InviteURL string `json:"invite_url"`
		StatusURL string `json:"status_url"`
		ExpiresAt string `json:"expires_at"`
	}
	decodeResponse(t, response, &result)
	if !strings.HasPrefix(result.InviteURL, testOrigin+"/#/invite/") || !strings.HasPrefix(result.StatusURL, testOrigin+"/#/status/") {
		t.Fatalf("generated URLs are not clean hash routes: %+v", result)
	}
	inviteParts := strings.Split(result.InviteURL, "/")
	statusParts := strings.Split(result.StatusURL, "/")
	expires, err := time.Parse(time.RFC3339, result.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return inviteParts[len(inviteParts)-1], statusParts[len(statusParts)-1], expires
}

func TestTokenGenerationAndHashing(t *testing.T) {
	token, hash, err := newToken(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 22 {
		t.Fatalf("token length = %d", len(token))
	}
	parsed, ok := hashToken(token)
	if !ok || parsed != hash || hash != sha256.Sum256([]byte(token)) {
		t.Fatal("token hash mismatch")
	}
	for _, invalid := range []string{"", token + "x", strings.Repeat("!", 22), token[:21] + "="} {
		if _, ok := hashToken(invalid); ok {
			t.Fatalf("accepted invalid token %q", invalid)
		}
	}
	if _, _, err := newToken(iotest.ErrReader(io.ErrUnexpectedEOF)); err == nil {
		t.Fatal("expected random source error")
	}
}

func TestValidation(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC)
	valid := validCreate(now)
	if err := validateCreate(valid, now); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*createRequest)
	}{
		{"empty name", func(r *createRequest) { r.AskerName = " " }},
		{"long name", func(r *createRequest) { r.RecipientName = strings.Repeat("é", 61) }},
		{"no ideas", func(r *createRequest) { r.OfferedIdeas = nil }},
		{"unknown idea", func(r *createRequest) { r.OfferedIdeas = []string{"moon"} }},
		{"duplicate idea", func(r *createRequest) { r.OfferedIdeas = []string{"pizza", "pizza"} }},
		{"past slot", func(r *createRequest) { r.ProposedSlots = []string{now.Add(-time.Second).Format(time.RFC3339)} }},
		{"far slot", func(r *createRequest) {
			r.ProposedSlots = []string{now.Add(8*24*time.Hour + time.Second).Format(time.RFC3339)}
		}},
		{"bad slot", func(r *createRequest) { r.ProposedSlots = []string{"tomorrow"} }},
		{"too many slots", func(r *createRequest) { r.ProposedSlots = []string{"1", "2", "3", "4", "5", "6"} }},
		{"invalid custom emoji", func(r *createRequest) { r.CustomIdeas = []customIdeaInput{{Emoji: "🌙", Title: "Moonlight"}} }},
		{"empty custom title", func(r *createRequest) { r.CustomIdeas = []customIdeaInput{{Emoji: "🎬", Title: " "}} }},
		{"long custom title", func(r *createRequest) {
			r.CustomIdeas = []customIdeaInput{{Emoji: "🎬", Title: strings.Repeat("界", 61)}}
		}},
		{"duplicate custom title", func(r *createRequest) {
			r.CustomIdeas = []customIdeaInput{{Emoji: "🎬", Title: "Movie"}, {Emoji: "🎨", Title: " movie "}}
		}},
		{"long sender message", func(r *createRequest) { r.SenderMessage = strings.Repeat("界", 281) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			copy := valid
			copy.OfferedIdeas = append([]string(nil), valid.OfferedIdeas...)
			copy.CustomIdeas = append([]customIdeaInput(nil), valid.CustomIdeas...)
			copy.ProposedSlots = append([]string(nil), valid.ProposedSlots...)
			tc.mutate(&copy)
			if err := validateCreate(copy, now); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	customOnly := valid
	customOnly.OfferedIdeas = nil
	customOnly.CustomIdeas = []customIdeaInput{{Emoji: "🎬", Title: " Movie night "}}
	customOnly.SenderMessage = " A personal note\nwith two lines. "
	if err := validateCreate(customOnly, now); err != nil {
		t.Fatalf("custom-only invite rejected: %v", err)
	}
	rec := inviteRecord{OfferedIdeas: []string{"pizza"}, CustomIdeas: []customIdea{{ID: "custom:0", Emoji: "🎬", Title: "Movie"}}, ProposedSlots: []string{"slot"}}
	zero, one := 0, 1
	for name, input := range map[string]acceptRequest{
		"empty":                  {SelectedSlotIndex: &zero},
		"unknown idea":           {SelectedIdeas: []string{"ramen"}, SelectedSlotIndex: &zero},
		"unknown custom idea":    {SelectedIdeas: []string{"custom:1"}, SelectedSlotIndex: &zero},
		"duplicate":              {SelectedIdeas: []string{"pizza", "pizza"}, SelectedSlotIndex: &zero},
		"bad slot":               {SelectedIdeas: []string{"pizza"}, SelectedSlotIndex: &one},
		"long custom":            {CustomIdea: strings.Repeat("界", 121), SelectedSlotIndex: &zero},
		"long recipient message": {SelectedIdeas: []string{"pizza"}, SelectedSlotIndex: &zero, RecipientMessage: strings.Repeat("界", 281)},
	} {
		t.Run("accept "+name, func(t *testing.T) {
			if validateAcceptance(input, rec) == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if err := validateAcceptance(acceptRequest{CustomIdea: "Arcade", SelectedSlotIndex: &zero}, rec); err != nil {
		t.Fatal(err)
	}
	if err := validateAcceptance(acceptRequest{SelectedIdeas: []string{"custom:0"}, SelectedSlotIndex: &zero}, rec); err != nil {
		t.Fatal(err)
	}
	if err := validateAcceptance(acceptRequest{SelectedIdeas: []string{"pizza"}, SelectedSlotIndex: &zero, RecipientMessage: " See you there! "}, rec); err != nil {
		t.Fatal(err)
	}
}

func TestRemovePronounMigrationPreservesInvites(t *testing.T) {
	db, err := openDatabase(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	initialSchema, err := embeddedFiles.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(initialSchema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (1, ?)`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	recipientToken, recipientHash, err := newToken(bytes.NewReader(bytes.Repeat([]byte{1}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	statusToken, statusHash, err := newToken(bytes.NewReader(bytes.Repeat([]byte{2}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC)
	acceptedAt := now.Add(time.Hour)
	expiresAt := now.Add(initialLifetime + extendedLifetime)
	if _, err := db.Exec(`INSERT INTO invites (
		recipient_token_hash, status_token_hash, asker_name, recipient_name, pronoun,
		offered_ideas, proposed_slots, created_at, accepted_at, expires_at,
		selected_ideas, custom_idea, selected_slot_index
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, recipientHash[:], statusHash[:], "Alex", "Taylor", "them", `["pizza"]`, `["2030-01-02T14:00:00Z"]`, now.Unix(), acceptedAt.Unix(), expiresAt.Unix(), `["pizza"]`, "Arcade", 0); err != nil {
		t.Fatal(err)
	}

	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	var pronounColumns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('invites') WHERE name = 'pronoun'`).Scan(&pronounColumns); err != nil {
		t.Fatal(err)
	}
	if pronounColumns != 0 {
		t.Fatal("pronoun column still exists after migration")
	}

	a := newApp(db, config{}, embeddedFiles, func() time.Time { return now }, rand.Reader)
	record, err := a.findInvite(context.Background(), "recipient_token_hash", recipientToken)
	if err != nil {
		t.Fatal(err)
	}
	if record.AskerName != "Alex" || record.RecipientName != "Taylor" || len(record.OfferedIdeas) != 1 || record.OfferedIdeas[0] != "pizza" ||
		len(record.CustomIdeas) != 0 || record.SenderMessage != "" || record.RecipientMessage != "" ||
		len(record.ProposedSlots) != 1 || record.ProposedSlots[0] != "2030-01-02T14:00:00Z" || record.AcceptedAt == nil || !record.AcceptedAt.Equal(acceptedAt) ||
		!record.ExpiresAt.Equal(expiresAt) || len(record.SelectedIdeas) != 1 || record.SelectedIdeas[0] != "pizza" || record.CustomIdea != "Arcade" ||
		record.SelectedSlotIndex == nil || *record.SelectedSlotIndex != 0 {
		t.Fatalf("invite changed during migration: %+v", record)
	}
	if _, err := a.findInvite(context.Background(), "status_token_hash", statusToken); err != nil {
		t.Fatalf("status token did not survive migration: %v", err)
	}
}

func TestClientIPSelectionAndHMAC(t *testing.T) {
	tests := []struct{ name, remote, forwarded, want string }{
		{"direct", "198.51.100.7:99", "203.0.113.1", "198.51.100.7"},
		{"trusted proxy", "127.0.0.1:99", "203.0.113.1, 10.0.0.2", "203.0.113.1"},
		{"rightmost untrusted", "10.0.0.1:99", "198.51.100.2, 192.168.1.2, 203.0.113.9", "203.0.113.9"},
		{"mapped", "[::ffff:192.0.2.4]:99", "", "192.0.2.4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("X-Forwarded-For", tc.forwarded)
			if got := clientIP(req).String(); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	now := time.Now()
	a := testApp(t, &now)
	h1 := a.ipHash(netip.MustParseAddr("192.0.2.1"))
	h2 := a.ipHash(netip.MustParseAddr("192.0.2.2"))
	if bytes.Equal(h1, h2) || bytes.Contains(h1, []byte("192.0.2.1")) {
		t.Fatal("IP HMAC is not opaque")
	}
}

func TestAPIIntegrationLifecycle(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC)
	a := testApp(t, &now)
	handler := a.routes()
	input := validCreate(now)
	input.CustomIdeas = []customIdeaInput{{Emoji: "🎬", Title: " Movie night "}}
	input.SenderMessage = " Dinner first?\nThen a movie! "
	inviteToken, statusToken, initialExpiry := createTokens(t, handler, now, input)
	if !initialExpiry.Equal(now.Add(initialLifetime)) {
		t.Fatalf("initial expiry = %v", initialExpiry)
	}

	view := request(t, handler, http.MethodPost, "/api/invites/view", tokenRequest{Token: inviteToken})
	if view.Code != http.StatusOK {
		t.Fatalf("view status %d: %s", view.Code, view.Body.String())
	}
	if view.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("API response is cacheable")
	}
	if view.Header().Get("Content-Security-Policy") == "" || view.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("missing security headers")
	}
	var invite map[string]any
	decodeResponse(t, view, &invite)
	if _, leaked := invite["status_token"]; leaked {
		t.Fatal("recipient response leaked status token")
	}
	if _, present := invite["pronoun"]; present {
		t.Fatal("recipient response contains removed pronoun field")
	}
	customIdeas, ok := invite["custom_ideas"].([]any)
	if !ok || len(customIdeas) != 1 || invite["sender_message"] != strings.TrimSpace(input.SenderMessage) {
		t.Fatalf("missing invite customization: %#v", invite)
	}
	customObject, ok := customIdeas[0].(map[string]any)
	if !ok || customObject["id"] != "custom:0" || customObject["emoji"] != "🎬" || customObject["title"] != "Movie night" {
		t.Fatalf("bad custom idea: %#v", customIdeas[0])
	}

	invalidAccept := request(t, handler, http.MethodPost, "/api/invites/accept", acceptRequest{Token: inviteToken})
	if invalidAccept.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid accept = %d", invalidAccept.Code)
	}
	index := 1
	recipientMessage := " Sounds perfect!\nSee you then. "
	accepted := request(t, handler, http.MethodPost, "/api/invites/accept", acceptRequest{Token: inviteToken, SelectedIdeas: []string{"coffee", "custom:0"}, CustomIdea: "Arcade", SelectedSlotIndex: &index, RecipientMessage: recipientMessage})
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept status %d: %s", accepted.Code, accepted.Body.String())
	}
	var acceptedResult struct {
		ExpiresAt string `json:"expires_at"`
	}
	decodeResponse(t, accepted, &acceptedResult)
	acceptedExpiry, _ := time.Parse(time.RFC3339, acceptedResult.ExpiresAt)
	if !acceptedExpiry.Equal(now.Add(initialLifetime + extendedLifetime)) {
		t.Fatalf("accepted expiry = %v", acceptedExpiry)
	}

	duplicate := request(t, handler, http.MethodPost, "/api/invites/accept", acceptRequest{Token: inviteToken, SelectedIdeas: []string{"coffee"}, SelectedSlotIndex: &index})
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d", duplicate.Code)
	}
	status := request(t, handler, http.MethodPost, "/api/status/view", tokenRequest{Token: statusToken})
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", status.Code, status.Body.String())
	}
	var statusResult struct {
		Status            string       `json:"status"`
		SelectedIdeas     []string     `json:"selected_ideas"`
		CustomIdeas       []customIdea `json:"custom_ideas"`
		CustomIdea        string       `json:"custom_idea"`
		RecipientMessage  string       `json:"recipient_message"`
		SelectedSlotIndex int          `json:"selected_slot_index"`
		ExpiresAt         string       `json:"expires_at"`
	}
	decodeResponse(t, status, &statusResult)
	if statusResult.Status != "accepted" || statusResult.CustomIdea != "Arcade" || statusResult.SelectedSlotIndex != 1 || len(statusResult.SelectedIdeas) != 2 ||
		len(statusResult.CustomIdeas) != 1 || statusResult.CustomIdeas[0].ID != "custom:0" || statusResult.RecipientMessage != strings.TrimSpace(recipientMessage) {
		t.Fatalf("bad status: %+v", statusResult)
	}

	deleted := request(t, handler, http.MethodPost, "/api/status/delete", tokenRequest{Token: statusToken})
	if deleted.Code != http.StatusNoContent || deleted.Body.Len() != 0 {
		t.Fatalf("delete = %d %q", deleted.Code, deleted.Body.String())
	}
	for endpoint, token := range map[string]string{"/api/invites/view": inviteToken, "/api/status/view": statusToken, "/api/status/delete": statusToken} {
		response := request(t, handler, http.MethodPost, endpoint, tokenRequest{Token: token})
		if response.Code != http.StatusNotFound || strings.TrimSpace(response.Body.String()) != `{"error":"not found"}` {
			t.Fatalf("%s after delete = %d %s", endpoint, response.Code, response.Body.String())
		}
	}
}

func TestConcurrentAcceptanceExactlyOnce(t *testing.T) {
	now := time.Date(2030, 2, 1, 9, 0, 0, 0, time.UTC)
	a := testApp(t, &now)
	handler := a.routes()
	inviteToken, _, _ := createTokens(t, handler, now, validCreate(now))
	index := 0
	const workers = 24
	var successes, conflicts, unexpected atomic.Int32
	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			response := request(t, handler, http.MethodPost, "/api/invites/accept", acceptRequest{Token: inviteToken, SelectedIdeas: []string{"pizza"}, SelectedSlotIndex: &index})
			switch response.Code {
			case http.StatusOK:
				successes.Add(1)
			case http.StatusConflict:
				conflicts.Add(1)
			default:
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if successes.Load() != 1 || conflicts.Load() != workers-1 || unexpected.Load() != 0 {
		t.Fatalf("success=%d conflict=%d unexpected=%d", successes.Load(), conflicts.Load(), unexpected.Load())
	}
}

func TestExpiryCleanupAndGenericNotFound(t *testing.T) {
	now := time.Date(2030, 3, 1, 9, 0, 0, 0, time.UTC)
	a := testApp(t, &now)
	handler := a.routes()
	inviteToken, statusToken, _ := createTokens(t, handler, now, validCreate(now))
	now = now.Add(initialLifetime)
	for endpoint, token := range map[string]string{"/api/invites/view": inviteToken, "/api/status/view": statusToken} {
		response := request(t, handler, http.MethodPost, endpoint, tokenRequest{Token: token})
		if response.Code != http.StatusNotFound || strings.TrimSpace(response.Body.String()) != `{"error":"not found"}` {
			t.Fatalf("expired %s = %d %s", endpoint, response.Code, response.Body.String())
		}
	}
	if err := a.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM invites`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invite count=%d err=%v", count, err)
	}
	if _, err := a.db.Exec(`INSERT INTO creation_events(ip_hash, created_at) VALUES (?, ?)`, make([]byte, 32), now.Add(-49*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := a.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM creation_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("event count=%d err=%v", count, err)
	}
}

func TestRateLimitsOriginBodyAndUnknownFields(t *testing.T) {
	now := time.Date(2030, 4, 1, 9, 0, 0, 0, time.UTC)
	a := testApp(t, &now)
	a.cfg.CreateHourlyLimit = 2
	handler := a.routes()
	legacyCreate := map[string]any{
		"asker_name": "Alex", "recipient_name": "Taylor", "pronoun": "them",
		"offered_ideas":  []string{"pizza"},
		"proposed_slots": []string{now.Add(2 * time.Hour).Format(time.RFC3339)},
	}
	legacyResponse := request(t, handler, http.MethodPost, "/api/invites", legacyCreate)
	if legacyResponse.Code != http.StatusBadRequest {
		t.Fatalf("legacy pronoun field = %d: %s", legacyResponse.Code, legacyResponse.Body.String())
	}
	for i, want := range []int{http.StatusCreated, http.StatusCreated, http.StatusTooManyRequests} {
		response := request(t, handler, http.MethodPost, "/api/invites", validCreate(now))
		if response.Code != want {
			t.Fatalf("create %d = %d: %s", i, response.Code, response.Body.String())
		}
		if want == http.StatusTooManyRequests && response.Header().Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
	}

	badOrigin := httptest.NewRequest(http.MethodPost, "/api/invites/view", strings.NewReader(`{"token":"anything"}`))
	badOrigin.Header.Set("Origin", "https://evil.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, badOrigin)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("bad origin = %d", recorder.Code)
	}

	unknown := httptest.NewRequest(http.MethodPost, "/api/invites/view", strings.NewReader(`{"token":"anything","extra":true}`))
	unknown.Header.Set("Origin", testOrigin)
	unknown.RemoteAddr = "198.51.100.3:1"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, unknown)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", recorder.Code)
	}

	large := httptest.NewRequest(http.MethodPost, "/api/invites/view", strings.NewReader(`{"token":"`+strings.Repeat("a", maxRequestBody)+`"}`))
	large.Header.Set("Origin", testOrigin)
	large.RemoteAddr = "198.51.100.4:1"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, large)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("large body = %d", recorder.Code)
	}
}

func TestDisabledRateLimits(t *testing.T) {
	now := time.Date(2030, 4, 2, 9, 0, 0, 0, time.UTC)
	a := testApp(t, &now)
	a.cfg.DisableRateLimits = true
	a.cfg.CreateHourlyLimit = 1
	a.cfg.CreateDailyLimit = 1
	a.cfg.GlobalDailyLimit = 1
	a.cfg.ViewMinuteLimit = 1
	handler := a.routes()

	for i := 0; i < 3; i++ {
		response := request(t, handler, http.MethodPost, "/api/invites", validCreate(now))
		if response.Code != http.StatusCreated {
			t.Fatalf("create %d = %d: %s", i, response.Code, response.Body.String())
		}
	}
	for i := 0; i < 3; i++ {
		response := request(t, handler, http.MethodPost, "/api/invites/view", tokenRequest{Token: "invalid"})
		if response.Code != http.StatusNotFound {
			t.Fatalf("view %d = %d: %s", i, response.Code, response.Body.String())
		}
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM creation_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("creation event count=%d err=%v", count, err)
	}
}

func TestLoadConfigDisabledRateLimits(t *testing.T) {
	t.Setenv("DISABLE_RATE_LIMITS", "true")
	t.Setenv("RATE_LIMIT_HMAC_KEY", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DisableRateLimits {
		t.Fatal("rate limits were not disabled")
	}

	t.Setenv("DISABLE_RATE_LIMITS", "not-a-boolean")
	if _, err := loadConfig(); err == nil {
		t.Fatal("invalid boolean was accepted")
	}
}

func TestXSSFieldsAreJSONEscapedAndRoundTrip(t *testing.T) {
	now := time.Date(2030, 5, 1, 9, 0, 0, 0, time.UTC)
	a := testApp(t, &now)
	handler := a.routes()
	payload := `<img src=x onerror=alert(1)>`
	input := validCreate(now)
	input.AskerName = payload
	input.RecipientName = `<script>alert(2)</script>`
	input.SenderMessage = payload
	input.CustomIdeas = []customIdeaInput{{Emoji: "🎬", Title: payload}}
	inviteToken, statusToken, _ := createTokens(t, handler, now, input)
	view := request(t, handler, http.MethodPost, "/api/invites/view", tokenRequest{Token: inviteToken})
	if bytes.Contains(view.Body.Bytes(), []byte("<script")) || bytes.Contains(view.Body.Bytes(), []byte("<img")) {
		t.Fatalf("JSON did not HTML-escape text: %s", view.Body.String())
	}
	var data createRequest
	decodeResponse(t, view, &data)
	if data.AskerName != payload || data.RecipientName != input.RecipientName || data.SenderMessage != payload || len(data.CustomIdeas) != 1 || data.CustomIdeas[0].Title != payload {
		t.Fatal("text did not round-trip")
	}
	index := 0
	accept := request(t, handler, http.MethodPost, "/api/invites/accept", acceptRequest{Token: inviteToken, CustomIdea: payload, SelectedSlotIndex: &index, RecipientMessage: payload})
	if accept.Code != http.StatusOK {
		t.Fatalf("accept = %d: %s", accept.Code, accept.Body.String())
	}
	status := request(t, handler, http.MethodPost, "/api/status/view", tokenRequest{Token: statusToken})
	if bytes.Contains(status.Body.Bytes(), []byte("<img")) {
		t.Fatalf("response text not escaped: %s", status.Body.String())
	}
	var statusData struct {
		RecipientMessage string `json:"recipient_message"`
	}
	decodeResponse(t, status, &statusData)
	if statusData.RecipientMessage != payload {
		t.Fatal("recipient message did not round-trip")
	}
}

func TestHealthAndStaticAssets(t *testing.T) {
	now := time.Now().UTC()
	handler := testApp(t, &now).routes()
	health := request(t, handler, http.MethodGet, "/healthz", nil)
	if health.Code != http.StatusOK || health.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("health = %d", health.Code)
	}
	index := request(t, handler, http.MethodGet, "/", nil)
	if index.Code != http.StatusOK ||
		!strings.Contains(index.Body.String(), `<script src="/app.js?v=14" defer></script>`) ||
		!strings.Contains(index.Body.String(), `<link rel="stylesheet" href="/styles.css?v=18">`) ||
		!strings.Contains(index.Body.String(), `<link rel="icon" href="/favicon.ico?v=1" sizes="32x32">`) ||
		!strings.Contains(index.Body.String(), `<link rel="icon" href="/favicon.svg?v=1" type="image/svg+xml" sizes="any">`) {
		t.Fatalf("static index = %d", index.Code)
	}
	for _, marker := range []string{
		`id="share-instructions"`,
		`id="share-invite-label"`,
		`id="share-status-label">PRIVATE STATUS LINK - VIEW RESPONSE HERE`,
		`class="private-link-warning-icon" aria-hidden="true"`,
		`<strong>KEEP THIS PRIVATE</strong>`,
		`If you lose this link, you won’t be able to recover it.`,
		`id="copy-status-btn">Copy Private Status Link 🔒`,
		`class="share-tertiary-actions"`,
		`class="btn btn-tertiary" id="preview-btn"`,
		`class="btn btn-tertiary" id="share-back-btn">← Create New Invite`,
		`id="recipient-message-label">3. Message (Optional)`,
		`id="recipient-message" maxlength="280"`,
	} {
		if !strings.Contains(index.Body.String(), marker) {
			t.Fatalf("generated links UI is missing marker %q", marker)
		}
	}
	faviconICO := request(t, handler, http.MethodGet, "/favicon.ico", nil)
	if faviconICO.Code != http.StatusOK || faviconICO.Header().Get("Content-Type") != "image/vnd.microsoft.icon" {
		t.Fatalf("ICO favicon = %d, content type = %q", faviconICO.Code, faviconICO.Header().Get("Content-Type"))
	}
	favicon := request(t, handler, http.MethodGet, "/favicon.svg", nil)
	if favicon.Code != http.StatusOK || favicon.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("favicon = %d, content type = %q", favicon.Code, favicon.Header().Get("Content-Type"))
	}
	if strings.Contains(index.Body.String(), "recipient-pronoun") {
		t.Fatal("static index still contains pronoun selector")
	}
	for _, marker := range []string{"rel=\"manifest\"", "apple-touch-icon", "mobile-web-app", "theme-color", "install-modal"} {
		if strings.Contains(index.Body.String(), marker) {
			t.Fatalf("static index still contains PWA marker %q", marker)
		}
	}
	client, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	clientText := string(client)
	if strings.Contains(clientText, ".pronoun") || !strings.Contains(clientText, "wants to take you out! Pick your ideal date:") {
		t.Fatal("client still depends on pronouns or is missing neutral invite copy")
	}
	for _, marker := range []string{"INVITE LINK - SEND THIS TO", "PRIVATE STATUS LINK - VIEW RESPONSE FROM", "Send the invite link to ${recipientName}", "startNewInvite", "${location.origin}/#/invite/", "${location.origin}/#/status/"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("generated links behavior is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"creator-preview-btn", "preview-generate-btn", "custom_ideas", "sender_message"} {
		if !strings.Contains(clientText, marker) && !strings.Contains(index.Body.String(), marker) {
			t.Fatalf("custom invite UI is missing marker %q", marker)
		}
	}
	if strings.Contains(clientText, "serviceWorker.register") || !strings.Contains(clientText, "serviceWorker.getRegistrations") {
		t.Fatal("client does not clean up legacy service workers")
	}
	for _, marker := range []string{"setupInstallPrompt", "beforeinstallprompt", "appinstalled"} {
		if strings.Contains(clientText, marker) {
			t.Fatalf("client still contains PWA marker %q", marker)
		}
	}
	tombstone := request(t, handler, http.MethodGet, "/service-worker.js", nil)
	if tombstone.Code != http.StatusOK || !strings.Contains(tombstone.Body.String(), "registration.unregister") || strings.Contains(tombstone.Body.String(), "addEventListener('fetch'") {
		t.Fatalf("legacy service worker tombstone is missing or unsafe: %d %s", tombstone.Code, tombstone.Body.String())
	}
	for _, asset := range []string{
		"/manifest.webmanifest", "/icon-192.png", "/icon-512.png",
		"/icon-maskable-512.png", "/apple-touch-icon.png", "/icon.svg", "/icon-maskable.svg",
	} {
		response := request(t, handler, http.MethodGet, asset, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("removed PWA asset %s = %d, want %d", asset, response.Code, http.StatusNotFound)
		}
	}
}

func TestMemoryLimiter(t *testing.T) {
	limiter := newMemoryLimiter()
	now := time.Now()
	if ok, _ := limiter.allow("key", now, 2, time.Minute); !ok {
		t.Fatal("first denied")
	}
	if ok, _ := limiter.allow("key", now.Add(time.Second), 2, time.Minute); !ok {
		t.Fatal("second denied")
	}
	if ok, retry := limiter.allow("key", now.Add(2*time.Second), 2, time.Minute); ok || retry < 1 {
		t.Fatalf("third ok=%v retry=%d", ok, retry)
	}
	if ok, _ := limiter.allow("key", now.Add(time.Minute+time.Second), 2, time.Minute); !ok {
		t.Fatal("expired event was not removed")
	}
}
