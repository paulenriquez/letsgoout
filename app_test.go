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
	if err := applyMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	cfg := config{
		PublicBaseURL: testOrigin, DatabasePath: "unused", RateLimitHMACKey: []byte("0123456789abcdef0123456789abcdef"),
		CreateHourlyLimit: 5, CreateDailyLimit: 20, GlobalDailyLimit: 500, ViewMinuteLimit: 1000, AcceptMinuteLimit: 1000,
	}
	return newApp(db, cfg, staticFiles, func() time.Time { return *now }, rand.Reader)
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
	recipientToken, recipientHash, ok := recipientTokenForStatus(token)
	if !ok || len(recipientToken) != 22 || recipientHash != sha256.Sum256([]byte(recipientToken)) {
		t.Fatal("could not derive recipient token from status token")
	}
	repeatedToken, repeatedHash, ok := recipientTokenForStatus(token)
	if !ok || repeatedToken != recipientToken || repeatedHash != recipientHash {
		t.Fatal("recipient token derivation is not deterministic")
	}
	if _, _, ok := recipientTokenForStatus("invalid"); ok {
		t.Fatal("derived a recipient token from an invalid status token")
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
		"multiple ideas":         {SelectedIdeas: []string{"pizza", "custom:0"}, SelectedSlotIndex: &zero},
		"offered idea and other": {SelectedIdeas: []string{"pizza"}, CustomIdea: "Arcade", SelectedSlotIndex: &zero},
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

	initialSchema, err := migrationFiles.ReadFile("migrations/001_initial.sql")
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

	if err := applyMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	var pronounColumns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('invites') WHERE name = 'pronoun'`).Scan(&pronounColumns); err != nil {
		t.Fatal(err)
	}
	if pronounColumns != 0 {
		t.Fatal("pronoun column still exists after migration")
	}

	a := newApp(db, config{}, staticFiles, func() time.Time { return now }, rand.Reader)
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
	if invite["status"] != "pending" {
		t.Fatalf("new invite status = %#v", invite["status"])
	}
	if _, leaked := invite["status_token"]; leaked {
		t.Fatal("recipient response leaked status token")
	}
	for _, field := range []string{"selected_ideas", "custom_idea", "selected_slot_index", "recipient_message"} {
		if _, present := invite[field]; present {
			t.Fatalf("pending recipient response contains accepted field %q", field)
		}
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
	multipleIdeas := request(t, handler, http.MethodPost, "/api/invites/accept", acceptRequest{Token: inviteToken, SelectedIdeas: []string{"coffee", "custom:0"}, SelectedSlotIndex: &index})
	if multipleIdeas.Code != http.StatusUnprocessableEntity {
		t.Fatalf("multiple-idea accept = %d: %s", multipleIdeas.Code, multipleIdeas.Body.String())
	}
	accepted := request(t, handler, http.MethodPost, "/api/invites/accept", acceptRequest{Token: inviteToken, SelectedIdeas: []string{}, CustomIdea: "Arcade", SelectedSlotIndex: &index, RecipientMessage: recipientMessage})
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
	acceptedView := request(t, handler, http.MethodPost, "/api/invites/view", tokenRequest{Token: inviteToken})
	if acceptedView.Code != http.StatusOK {
		t.Fatalf("accepted invite view = %d: %s", acceptedView.Code, acceptedView.Body.String())
	}
	var acceptedInvite struct {
		Status            string   `json:"status"`
		SelectedIdeas     []string `json:"selected_ideas"`
		CustomIdea        string   `json:"custom_idea"`
		SelectedSlotIndex int      `json:"selected_slot_index"`
		RecipientMessage  string   `json:"recipient_message"`
	}
	decodeResponse(t, acceptedView, &acceptedInvite)
	if acceptedInvite.Status != "accepted" || len(acceptedInvite.SelectedIdeas) != 0 || acceptedInvite.CustomIdea != "Arcade" ||
		acceptedInvite.SelectedSlotIndex != index || acceptedInvite.RecipientMessage != strings.TrimSpace(recipientMessage) {
		t.Fatalf("bad accepted recipient view: %+v", acceptedInvite)
	}
	var acceptedPayload map[string]any
	decodeResponse(t, acceptedView, &acceptedPayload)
	for _, field := range []string{"status_token", "status_url", "accepted_at"} {
		if _, leaked := acceptedPayload[field]; leaked {
			t.Fatalf("accepted recipient response leaked private field %q", field)
		}
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
		InviteURL         string       `json:"invite_url"`
	}
	decodeResponse(t, status, &statusResult)
	if statusResult.Status != "accepted" || statusResult.CustomIdea != "Arcade" || statusResult.SelectedSlotIndex != 1 || len(statusResult.SelectedIdeas) != 0 ||
		len(statusResult.CustomIdeas) != 1 || statusResult.CustomIdeas[0].ID != "custom:0" || statusResult.RecipientMessage != strings.TrimSpace(recipientMessage) ||
		statusResult.InviteURL != testOrigin+"/#/invite/"+inviteToken {
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

func TestMigrationsAreNotServed(t *testing.T) {
	now := time.Now().UTC()
	handler := testApp(t, &now).routes()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, target := range []string{"/migrations/", "/migrations/001_initial.sql"} {
			response := request(t, handler, method, target, nil)
			if response.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want %d", method, target, response.Code, http.StatusNotFound)
			}
			body := response.Body.String()
			if strings.Contains(body, "CREATE TABLE") || strings.Contains(body, "001_initial.sql") {
				t.Errorf("%s %s exposed migration content: %q", method, target, body)
			}
		}
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
		!strings.Contains(index.Body.String(), `<script src="/app.js?v=72" defer></script>`) ||
		!strings.Contains(index.Body.String(), `<link rel="stylesheet" href="/styles.css?v=70">`) ||
		!strings.Contains(index.Body.String(), `<link rel="icon" href="/favicon.ico?v=1" sizes="32x32">`) ||
		!strings.Contains(index.Body.String(), `<link rel="icon" href="/favicon.svg?v=1" type="image/svg+xml" sizes="any">`) {
		t.Fatalf("static index = %d", index.Code)
	}
	for _, marker := range []string{
		`id="celebration-confetti" aria-hidden="true"`,
		`id="accepted-plan" role="status" aria-live="polite" aria-atomic="true"`,
		`id="accepted-ideas-icon" aria-hidden="true"`,
		`class="accepted-ideas-list" id="accepted-ideas"`,
		`id="accepted-slot"`,
		`id="accepted-message"`,
		`id="accepted-message-label"`,
		`id="accepted-message-text"`,
		`class="accepted-note">Can't wait, see you soon! 🤩`,
		`class="btn btn-primary accepted-replay-button" id="accepted-replay-confetti-btn">Show Emoji Confetti Again</button>`,
		`id="share-instructions"`,
		`<span class="form-label">INVITE LINK</span>`,
		`id="share-invite-label">Share this`,
		`id="generated-invite-box" aria-label="Copy invite link"`,
		`class="link-box-value" id="generated-invite-url"`,
		`<span class="form-label">PRIVATE STATUS LINK</span>`,
		`id="share-status-label">Save this link to view their response`,
		`id="generated-status-box" aria-label="Copy private status link"`,
		`class="link-box-value" id="generated-status-url"`,
		`class="private-link-warning-icon" aria-hidden="true"`,
		`<strong>For your eyes only.</strong>`,
		`Save it somewhere you won’t lose it.`,
		`It can’t be recovered.`,
		`id="copy-status-btn">Copy Private Status Link 🔒`,
		`class="share-tertiary-actions"`,
		`class="btn btn-tertiary" id="preview-btn"`,
		`class="btn btn-tertiary" id="share-back-btn">← Create New Invite`,
		`id="recipient-message-label">3. Message (Optional)`,
		`id="recipient-message" maxlength="280" rows="3" placeholder="Anything else you want the sender to know?"`,
		`class="status-invite-share hidden" id="status-invite-share"`,
		`id="status-invite-share-details"`,
		`class="btn btn-secondary status-revisit-invite-button hidden" id="status-revisit-invite-btn">Revisit Invite Link 👀</button>`,
		`class="accepted-plan status-response hidden" id="status-details" aria-label="Accepted response"`,
		`id="status-response-ideas-icon" aria-hidden="true"`,
		`class="accepted-ideas-list" id="status-response-ideas"`,
		`id="status-response-slot"`,
		`id="status-response-message"`,
		`id="status-response-message-label"`,
		`class="accepted-message-text" id="status-response-message-text"`,
		`id="status-invite-label">Share this`,
		`id="status-invite-box" aria-label="Copy invite link"`,
		`class="link-box-value" id="status-invite-url"`,
		`id="status-copy-invite-btn">Copy Invite Link 📋`,
		`id="other-freeform" maxlength="120" placeholder="Suggest something else…" aria-describedby="accept-error"`,
		`id="yes-btn" disabled>Accept</button>`,
		`id="status-accepted-row"`,
		`id="status-expires-row"`,
		`id="status-expires"`,
		`id="status-updated"`,
		`id="status-updated-row">
                <span>Last checked</span>`,
		`class="status-next-check hidden" id="status-next-check"`,
		`class="btn btn-tertiary danger-button" id="status-delete-btn">Permanently Delete Invite ❌`,
		`class="status-actions" id="status-actions"`,
	} {
		if !strings.Contains(index.Body.String(), marker) {
			t.Fatalf("generated links UI is missing marker %q", marker)
		}
	}
	if strings.Contains(index.Body.String(), `id="recipient-tagline"`) {
		t.Fatal("recipient invite still includes the removed tagline")
	}
	metadataIndex := strings.Index(index.Body.String(), `id="status-metadata"`)
	deleteIndex := strings.Index(index.Body.String(), `id="status-delete-btn"`)
	if metadataIndex < 0 || deleteIndex < metadataIndex {
		t.Fatal("private status delete action is not below the metadata")
	}
	faviconICO := request(t, handler, http.MethodGet, "/favicon.ico", nil)
	if faviconICO.Code != http.StatusOK || faviconICO.Header().Get("Content-Type") != "image/vnd.microsoft.icon" {
		t.Fatalf("ICO favicon = %d, content type = %q", faviconICO.Code, faviconICO.Header().Get("Content-Type"))
	}
	favicon := request(t, handler, http.MethodGet, "/favicon.svg", nil)
	if favicon.Code != http.StatusOK || favicon.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("favicon = %d, content type = %q", favicon.Code, favicon.Header().Get("Content-Type"))
	}
	statusCheck := request(t, handler, http.MethodGet, "/check-circle.svg", nil)
	if statusCheck.Code != http.StatusOK || statusCheck.Header().Get("Content-Type") != "image/svg+xml" ||
		!strings.Contains(statusCheck.Body.String(), `fill="#277a47"`) {
		t.Fatalf("status check icon = %d, content type = %q", statusCheck.Code, statusCheck.Header().Get("Content-Type"))
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
	if strings.Contains(clientText, ".pronoun") {
		t.Fatal("client still depends on pronouns")
	}
	for _, marker := range []string{
		"byID('recipient-title').textContent = `Hey ${data.recipient_name}!`;",
		`const senderMessage = data.sender_message || "Let's go out? Pick whatever sounds best, and I'll handle the rest! 💕";`,
		"byID('recipient-subtitle').textContent = senderMessage;",
		"byID('recipient-subtitle').classList.remove('hidden');",
	} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("sender message fallback is missing marker %q", marker)
		}
	}
	if strings.Contains(index.Body.String(), `id="recipient-title">Hey! 💕`) || strings.Contains(clientText, "`Hey ${data.recipient_name}! 💕`") {
		t.Fatal("recipient greeting still includes the removed heart emoji")
	}
	if !strings.Contains(clientText, "`Anything else you want ${data.asker_name} to know?`") {
		t.Fatal("recipient message placeholder does not include the sender name")
	}
	for _, marker := range []string{
		"data.offered_ideas.filter((id) => id !== 'any').forEach((id) => appendIdea(ideaByID.get(id)))",
		"data.custom_ideas.forEach(appendIdea)",
		"if (data.offered_ideas.includes('any')) appendIdea(ideaByID.get('any'))",
		"ideasGrid.appendChild(createIdeaCard(other, selectOther, !previewMode))",
	} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("recipient idea ordering is missing marker %q", marker)
		}
	}
	for _, marker := range []string{
		"card.setAttribute('aria-pressed', 'false')",
		"const selectOnlyIdea = (id, card) =>",
		"recipientSelectedIdeas.clear()",
		"candidate.classList.remove('selected')",
		"recipientSelectedIdeas.add(id)",
		"const selectOther = previewMode ? null : (card) => selectOnlyIdea('other', card)",
	} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("single recipient idea selection is missing marker %q", marker)
		}
	}
	if strings.Contains(index.Body.String(), "status-refresh-btn") || strings.Contains(clientText, "status-refresh-btn") {
		t.Fatal("private status page still has a manual refresh control")
	}
	for _, marker := range []string{"const statusRefreshInterval = 15000", "statusAutoRefreshEnabled = data.status === 'pending'", "if (!statusAutoRefreshEnabled || !currentStatusToken", "nextStatusRefreshAt = Date.now() + statusRefreshInterval", "`Checking again in ${seconds}s`", "statusRefreshTimer = window.setTimeout(refreshStatus, statusRefreshInterval)", "document.addEventListener('visibilitychange'", "else if (statusAutoRefreshEnabled) refreshStatus();", "window.addEventListener('online', () => { if (statusAutoRefreshEnabled) refreshStatus(); })", "window.addEventListener('offline'"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("automatic status refresh is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"summary.textContent = 'Still waiting for a response.'", "summary.classList.add('status-summary-pending')", "summary.classList.remove('status-summary-pending')"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("pending status emphasis is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"summary.classList.remove('status-summary-accepted')", "summary.classList.add('status-summary-accepted')", "makeElement('span', 'status-summary-confirmation')", "makeElement('img', 'status-summary-check')", "checkIcon.src = '/check-circle.svg?v=1'", "checkIcon.alt = ''", "checkIcon.setAttribute('aria-hidden', 'true')", `makeElement('span', '', "It's a date!")`, `makeElement('span', 'status-summary-caption', "Here's the accepted plan")`, "byID('status-summary').classList.remove('status-summary-accepted')"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("accepted status confirmation is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"function renderStatusError(message)", "byID('status-card').classList.add('status-error-state')", "byID('status-card').classList.remove('status-error-state')", "byID('status-summary').classList.add('hidden')", "byID('status-invite-share').classList.add('hidden')", "byID('status-expires-row').classList.add('hidden')", "byID('status-actions').classList.add('hidden')", "byID('status-updated-row').classList.remove('hidden')", "renderStatusError(error.message || 'Could not refresh the status.')", "renderStatusError('You appear to be offline.')", "summary.classList.remove('hidden')", "byID('status-expires-row').classList.remove('hidden')", "byID('status-actions').classList.remove('hidden')"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("status error state is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"if (error.status === 404)", "unavailableCard.classList.add('status-unavailable-state')", "unavailableCard.querySelector('h2').textContent = 'Invite Unavailable or Expired'", "unavailableCard.querySelector('p').classList.add('hidden')", "byID('unavailable-card').classList.remove('status-unavailable-state')", "byID('unavailable-card').querySelector('p').classList.remove('hidden')", "showScreen('unavailable-card')"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("status 404 page is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"Share this to ${recipientName}", "Save this link to view ${recipientName}'s response", "Share the invite link with ${recipientName}", "generated-invite-box').addEventListener('click', () => copyLink('generated-invite-url'", "generated-status-box').addEventListener('click', () => copyLink('generated-status-url'", "status-invite-box').addEventListener('click', () => copyLink('status-invite-url'", "status-copy-invite-btn').addEventListener('click', () => copyLink('status-invite-url'", "startNewInvite", "${location.origin}/#/invite/", "${location.origin}/#/status/"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("generated links behavior is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"currentStatusInviteURL = data.invite_url || ''", "inviteShareDetails.classList.toggle('hidden', data.status !== 'pending')", "revisitInviteButton.classList.toggle('hidden', data.status !== 'accepted')", "window.open(currentStatusInviteURL, '_blank', 'noopener,noreferrer')"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("status invite-link state is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"const celebrationEmojis = ['😍', '🥰', '♥️', '💘', '💗', '💓']", "const celebrationEmoji = celebrationEmojis[Math.floor(Math.random() * celebrationEmojis.length)]", "makeElement('span', 'confetti-piece', celebrationEmoji)", "recipientEmoji.textContent = '💖'", "${currentInvite.recipient_name}, it’s a date!", "Your response has been shared with ${currentInvite.asker_name}", "startCelebration", "clearCelebration", "window.innerWidth < 480 ? 160 : 240", "const waveSeconds = 1.2", "const minimumFallSeconds = 3", "const fallVarianceSeconds = 0.8", "const isHeroPiece = index % 5 === 0", "const horizontalSlot = index % 14", "startX = -4", "horizontalSlot < 3", "horizontalSlot > 10", "((index + Math.random()) / pieceCount) * (waveSeconds + duration)", "isHeroPiece ? 4.5 + Math.random() * 1.5 : 2.25 + Math.random() * 1.75", "piece.addEventListener('animationend', () => piece.remove(), { once: true })", "waveSeconds + maximumFallSeconds + 0.1", "(prefers-reduced-motion: reduce)"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("accepted celebration behavior is missing marker %q", marker)
		}
	}
	if strings.Contains(clientText, "accepted-popper") || strings.Contains(clientText, "accepted-sparkles") {
		t.Fatal("accepted celebration still renders the old popper and sparkles illustration")
	}
	for _, marker := range []string{"function selectedPlanIdea(selectedIdeas, customIdeas, customIdea)", "const selectedID = selectedIdeas[0]", "return { label: customIdea, emoji: customIdea ? '🤔' : '🚀' }", "ideaEmoji(selectedID, customIdeas)", "Your message to ${currentInvite.asker_name}", "classList.toggle('hidden', !recipientMessage)", "renderAccepted(choice, formatSlot(currentInvite.proposed_slots[slotIndex]), recipientMessage)", "renderAcceptedResponse(data)", "if (data.status === 'accepted') renderAcceptedResponse(data); else renderRecipientView(data)"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("accepted single choice or message behavior is missing marker %q", marker)
		}
	}
	if !strings.Contains(clientText, "byID('accepted-replay-confetti-btn').addEventListener('click', startCelebration)") {
		t.Fatal("accepted confetti replay button is not wired to the celebration")
	}
	for _, marker := range []string{"function renderPlanIdea(icon, list, choice)", "icon.textContent = choice.emoji || '🚀'", "makeElement('li', 'accepted-idea accepted-idea-single')", "makeElement('span', 'accepted-idea-label', choice.label)", "renderPlanIdea(byID('status-response-ideas-icon'), byID('status-response-ideas'), choice)", "byID('status-response-slot').textContent = formatSlot", "`Message from ${data.recipient_name}`", "byID('status-response-message').classList.toggle('hidden', !data.recipient_message)"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("accepted single idea display is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"const otherIncomplete = otherSelected && byID('other-freeform').value.trim().length === 0", "if (otherSelected && !customIdea)", "Tell us what you would prefer for “Other”.", "if (noButton.parentElement !== document.body) detachNoButtonForDodge()", "function detachNoButtonForDodge()", "if (!touch || previewMode || noButton.disabled) return", "const farthestDistance = Math.max(...candidateTargets.map(distanceTo))", "const minimumTravel = Math.min(120, farthestDistance)", "for (let attempt = 0; attempt < 40; attempt += 1)", "const acceptRect = byID('yes-btn').getBoundingClientRect()", "const overlapGap = 12", "const overlapsAccept = (target)", "const pathCrossesAccept = (target)", "const movingTargets = candidateTargets.filter((target) => distanceTo(target) >= minimumTravel)", "const nonOverlappingTargets = movingTargets.filter((target) => !overlapsAccept(target))", "? safeTargets", ": (nonOverlappingTargets.length > 0 ? nonOverlappingTargets : movingTargets)", "noButton.style.left = `${current.left + window.scrollX}px`", "noButton.style.top = `${current.top + window.scrollY}px`", "noButton.style.transform = `translate3d(${target.x - current.left}px, ${target.y - current.top}px, 0)`", "function settleDodge(event)", "noButton.style.left = `${settled.left + window.scrollX}px`", "function pointHitsNoButton(x, y)", "const touchPadding = 18", "if (event.pointerType !== 'mouse') return", "document.addEventListener('touchstart'", "pointHitsNoButton(touch.clientX, touch.clientY)", "{ capture: true, passive: false }", "noButton.addEventListener('pointerdown'", "noButton.addEventListener('transitionend', settleDodge)", "noButton.style.transform = 'translate3d(0, 0, 0)'", "document.body.appendChild(noButton)", "wrapper.appendChild(noButton)"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("recipient invite safeguards are missing marker %q", marker)
		}
	}
	if strings.Contains(clientText, "setupInitialNoButtonPosition") || strings.Count(clientText, "detachNoButtonForDodge();") != 1 {
		t.Fatal("decline button detaches before its first dodge")
	}
	if strings.Contains(clientText, "noButton.disabled || noButton.parentElement !== document.body") {
		t.Fatal("touch cannot trigger the first decline-button dodge from static layout")
	}
	if strings.Contains(clientText, "if (safeTargets.length === 0) return") {
		t.Fatal("decline button can still ignore a tap when no path-safe corner exists")
	}
	if strings.Contains(clientText, "noButton.addEventListener('touchstart'") {
		t.Fatal("decline button still uses the unreliable touch-only handler")
	}
	if strings.Contains(clientText, "window.addEventListener('scroll', prepareNoButtonPosition") {
		t.Fatal("decline button still resets while scrolling")
	}
	if strings.Contains(clientText, "window.addEventListener('resize', prepareNoButtonPosition") {
		t.Fatal("decline button still resets during scroll-related viewport resizing")
	}
	if !strings.Contains(clientText, "function dodge() {\n        if (previewMode) return;") {
		t.Fatal("decline button still dodges in read-only preview mode")
	}
	styles, err := os.ReadFile("styles.css")
	if err != nil {
		t.Fatal(err)
	}
	styleText := string(styles)
	if !strings.Contains(styleText, ".preview-actions {\n    display: flex; flex-direction: column; gap: 12px; margin: 1rem 0 0.5rem;") {
		t.Fatal("preview actions are missing the requested whitespace")
	}
	for _, marker := range []string{"-webkit-tap-highlight-color: transparent", "backface-visibility: hidden; touch-action: none", "transition: none; will-change: transform", "transition: transform 0.75s"} {
		if !strings.Contains(styleText, marker) {
			t.Fatalf("decline button Safari rendering safeguard is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"#recipient-card.accepted-state", "--accepted-content-gap: 1.4rem", "margin-bottom: var(--accepted-content-gap)", "margin: var(--accepted-content-gap) 0 0", "padding: 2.5rem 1.25rem 1.5rem", "font-size: 5.5rem; line-height: 1", "translateY(-3px)", ".accepted-plan-grid", ".accepted-idea-single { display: block; }", ".accepted-message-text", "font-size: 1.1rem; font-weight: 400", ".accepted-replay-button { margin: 2rem 0 0; }", ".status-summary-pending::before", ".status-summary-accepted", "margin-top: 0.65rem", ".status-summary-confirmation", "color: #277a47", ".status-summary-check { width: 1.25rem", ".status-summary-caption { color: var(--text-muted); font-weight: 400", ".status-response { margin-bottom: 1rem; }", ".status-revisit-invite-button { width: 100%; margin: 0; }", "#status-card { padding-bottom: 1.1rem; }", "border-bottom: 1px solid rgba(92, 67, 71, 0.16)", "#status-card.status-error-state #status-error { margin: 1rem 0 1.5rem; }", "#status-card.status-error-state .status-metadata", "margin-top: 0; padding: 0; border-top: 0; border-bottom: 0;", "#unavailable-card.status-unavailable-state #unavailable-create-btn { margin-top: 1.25rem; }", "@keyframes status-pulse", ".status-summary-pending::before { animation: none; }", "@keyframes celebration-bounce", "@keyframes emoji-confetti-fall", "@media (prefers-reduced-motion: reduce)"} {
		if !strings.Contains(styleText, marker) {
			t.Fatalf("accepted celebration styles are missing marker %q", marker)
		}
	}
	for _, marker := range []string{"creator-preview-btn", "preview-generate-btn", "custom_ideas", "sender_message"} {
		if !strings.Contains(clientText, marker) && !strings.Contains(index.Body.String(), marker) {
			t.Fatalf("custom invite UI is missing marker %q", marker)
		}
	}
	for _, marker := range []string{"function updateSlotControls()", "remove-slot-btn", "if (slotsWrapper.children.length <= 1) return", "remove.setAttribute('aria-label', `Remove date and time option ${index + 1}`)", "byID('add-slot-trigger').disabled = rows.length >= 5"} {
		if !strings.Contains(clientText, marker) {
			t.Fatalf("removable date option behavior is missing marker %q", marker)
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
