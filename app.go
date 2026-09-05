package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxRequestBody      = 8 << 10
	initialLifetime     = 7 * 24 * time.Hour
	extendedLifetime    = 7 * 24 * time.Hour
	maxCustomIdeaTitle  = 60
	maxSenderMessage    = 280
	maxRecipientMessage = 280
)

var (
	validIdeas = map[string]bool{
		"pizza": true, "ramen": true, "coffee": true, "drinks": true,
		"steak": true, "gym": true, "walk": true, "run": true, "any": true,
	}
)

const emojiCatalogPath = "emoji/emoji-data.json"

type app struct {
	db        *sql.DB
	cfg       config
	static    fs.FS
	now       func() time.Time
	random    io.Reader
	createMu  sync.Mutex
	emojis    map[string]struct{}
	emojiETag string
}

func newApp(db *sql.DB, cfg config, static fs.FS, now func() time.Time, random io.Reader) (*app, error) {
	emojis, etag, err := loadEmojiCatalog(static)
	if err != nil {
		return nil, fmt.Errorf("load emoji catalog: %w", err)
	}
	return &app{
		db: db, cfg: cfg, static: static, now: now, random: random,
		emojis: emojis, emojiETag: etag,
	}, nil
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/invites", a.handleCreate)
	mux.HandleFunc("POST /api/invites/view", a.handleInviteView)
	mux.HandleFunc("POST /api/invites/accept", a.handleAccept)
	mux.HandleFunc("POST /api/status/view", a.handleStatusView)
	mux.HandleFunc("POST /api/status/delete", a.handleStatusDelete)
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusNotFound, "not found") })
	mux.Handle("/", a.staticHandler())
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; worker-src 'self'; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), microphone=(), payment=(), usb=()")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/healthz" {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) staticHandler() http.Handler {
	files := http.FileServer(http.FS(a.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "." || clean == "" {
			clean = "index.html"
		}
		if _, err := fs.Stat(a.static, clean); err != nil {
			http.NotFound(w, r)
			return
		}
		if clean == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		if clean == emojiCatalogPath {
			w.Header().Set("ETag", a.emojiETag)
			if r.Header.Get("If-None-Match") == a.emojiETag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}

type emojiCatalogEntry struct {
	Emoji string             `json:"emoji"`
	Group int                `json:"group"`
	Skins []emojiCatalogSkin `json:"skins"`
}

type emojiCatalogSkin struct {
	Emoji string `json:"emoji"`
}

func loadEmojiCatalog(static fs.FS) (map[string]struct{}, string, error) {
	body, err := fs.ReadFile(static, emojiCatalogPath)
	if err != nil {
		return nil, "", err
	}
	var entries []emojiCatalogEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, "", err
	}
	if len(entries) == 0 {
		return nil, "", errors.New("catalog is empty")
	}
	emojis := make(map[string]struct{}, len(entries)*2)
	addEmoji := func(value string) error {
		if !utf8.ValidString(value) || value == "" {
			return errors.New("catalog contains an invalid emoji")
		}
		emojis[value] = struct{}{}
		// Preserve compatibility with previously stored minimally-qualified forms.
		withoutVariationSelector := strings.ReplaceAll(value, "\uFE0F", "")
		if withoutVariationSelector != "" {
			emojis[withoutVariationSelector] = struct{}{}
		}
		return nil
	}
	for _, entry := range entries {
		// Group 2 contains composition components such as standalone skin tones and hair.
		if entry.Group == 2 {
			continue
		}
		if err := addEmoji(entry.Emoji); err != nil {
			return nil, "", err
		}
		for _, skin := range entry.Skins {
			if err := addEmoji(skin.Emoji); err != nil {
				return nil, "", errors.New("catalog contains an invalid emoji variant")
			}
		}
	}
	if len(emojis) == 0 {
		return nil, "", errors.New("catalog has no selectable emoji")
	}
	digest := sha256.Sum256(body)
	return emojis, `"` + hex.EncodeToString(digest[:]) + `"`, nil
}

type createRequest struct {
	AskerName     string            `json:"asker_name"`
	RecipientName string            `json:"recipient_name"`
	OfferedIdeas  []string          `json:"offered_ideas"`
	CustomIdeas   []customIdeaInput `json:"custom_ideas"`
	SenderMessage string            `json:"sender_message"`
	ProposedSlots []string          `json:"proposed_slots"`
}

type customIdeaInput struct {
	Emoji string `json:"emoji"`
	Title string `json:"title"`
}

type customIdea struct {
	ID    string `json:"id"`
	Emoji string `json:"emoji"`
	Title string `json:"title"`
}

type tokenRequest struct {
	Token string `json:"token"`
}

type acceptRequest struct {
	Token             string   `json:"token"`
	SelectedIdeas     []string `json:"selected_ideas"`
	CustomIdea        string   `json:"custom_idea"`
	SelectedSlotIndex *int     `json:"selected_slot_index"`
	RecipientMessage  string   `json:"recipient_message"`
}

type inviteRecord struct {
	ID                int64
	AskerName         string
	RecipientName     string
	OfferedIdeas      []string
	CustomIdeas       []customIdea
	SenderMessage     string
	ProposedSlots     []string
	CreatedAt         time.Time
	AcceptedAt        *time.Time
	ExpiresAt         time.Time
	SelectedIdeas     []string
	CustomIdea        string
	SelectedSlotIndex *int
	RecipientMessage  string
}

func (a *app) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !a.validOrigin(w, r) {
		return
	}
	var req createRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	now := a.now().UTC()
	if err := validateCreate(req, now, a.emojis); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	ideasJSON, _ := json.Marshal(req.OfferedIdeas)
	customIdeasJSON, _ := json.Marshal(normalizeCustomIdeas(req.CustomIdeas))
	slotsJSON, _ := json.Marshal(req.ProposedSlots)
	expires := now.Add(initialLifetime)

	a.createMu.Lock()
	defer a.createMu.Unlock()
	allowed, retry, err := a.allowCreationLocked(r.Context(), now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !allowed {
		w.Header().Set("Retry-After", fmt.Sprint(retry))
		writeError(w, http.StatusTooManyRequests, "invite creation limit reached")
		return
	}
	statusToken, statusHash, err := newToken(a.random)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	recipientToken, recipientHash, ok := recipientTokenForStatus(statusToken)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO invites (
			recipient_token_hash, status_token_hash, asker_name, recipient_name,
			offered_ideas, custom_ideas, sender_message, proposed_slots, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, recipientHash[:], statusHash[:], strings.TrimSpace(req.AskerName), strings.TrimSpace(req.RecipientName), ideasJSON, customIdeasJSON, strings.TrimSpace(req.SenderMessage), slotsJSON, now.Unix(), expires.Unix())
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO creation_events(created_at) VALUES (?)`, now.Unix())
	}
	if err != nil {
		if tx != nil {
			tx.Rollback()
		}
		writeMutationError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeMutationError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"invite_url": a.cfg.PublicBaseURL + "/#/invite/" + recipientToken,
		"status_url": a.cfg.PublicBaseURL + "/#/status/" + statusToken,
		"expires_at": expires.Format(time.RFC3339),
	})
}

func (a *app) handleInviteView(w http.ResponseWriter, r *http.Request) {
	if !a.validOrigin(w, r) {
		return
	}
	var req tokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	record, err := a.findInvite(r.Context(), "recipient_token_hash", req.Token)
	if err != nil {
		writeNotFound(w)
		return
	}
	response := map[string]any{
		"status":     "pending",
		"asker_name": record.AskerName, "recipient_name": record.RecipientName,
		"offered_ideas": record.OfferedIdeas, "custom_ideas": record.CustomIdeas,
		"sender_message": record.SenderMessage, "proposed_slots": record.ProposedSlots,
		"expires_at": record.ExpiresAt.Format(time.RFC3339),
	}
	if record.AcceptedAt != nil {
		if record.SelectedSlotIndex == nil {
			writeNotFound(w)
			return
		}
		response["status"] = "accepted"
		response["selected_ideas"] = record.SelectedIdeas
		response["custom_idea"] = record.CustomIdea
		response["selected_slot_index"] = *record.SelectedSlotIndex
		response["recipient_message"] = record.RecipientMessage
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) handleAccept(w http.ResponseWriter, r *http.Request) {
	if !a.validOrigin(w, r) {
		return
	}
	var req acceptRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	record, err := a.findInvite(r.Context(), "recipient_token_hash", req.Token)
	if err != nil {
		writeNotFound(w)
		return
	}
	if record.AcceptedAt != nil {
		writeError(w, http.StatusConflict, "invitation already accepted")
		return
	}
	if err := validateAcceptance(req, record); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	selectedJSON, _ := json.Marshal(req.SelectedIdeas)
	now := a.now().UTC()
	result, err := a.db.ExecContext(r.Context(), `UPDATE invites SET accepted_at = ?, expires_at = expires_at + ?, selected_ideas = ?, custom_idea = ?, selected_slot_index = ?, recipient_message = ? WHERE id = ? AND accepted_at IS NULL AND expires_at > ?`, now.Unix(), int64(extendedLifetime/time.Second), selectedJSON, strings.TrimSpace(req.CustomIdea), *req.SelectedSlotIndex, strings.TrimSpace(req.RecipientMessage), record.ID, now.Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if rows == 0 {
		var accepted sql.NullInt64
		if err := a.db.QueryRowContext(r.Context(), `SELECT accepted_at FROM invites WHERE id = ?`, record.ID).Scan(&accepted); err == nil && accepted.Valid {
			writeError(w, http.StatusConflict, "invitation already accepted")
		} else {
			writeNotFound(w)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "accepted", "expires_at": record.ExpiresAt.Add(extendedLifetime).Format(time.RFC3339)})
}

func (a *app) handleStatusView(w http.ResponseWriter, r *http.Request) {
	if !a.validOrigin(w, r) {
		return
	}
	var req tokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	record, err := a.findInvite(r.Context(), "status_token_hash", req.Token)
	if err != nil {
		writeNotFound(w)
		return
	}
	response := map[string]any{
		"status": "pending", "asker_name": record.AskerName, "recipient_name": record.RecipientName,
		"custom_ideas": record.CustomIdeas, "proposed_slots": record.ProposedSlots,
		"expires_at": record.ExpiresAt.Format(time.RFC3339),
	}
	if recipientToken, recipientHash, ok := recipientTokenForStatus(req.Token); ok {
		var storedHash []byte
		if err := a.db.QueryRowContext(r.Context(), `SELECT recipient_token_hash FROM invites WHERE id = ?`, record.ID).Scan(&storedHash); err == nil &&
			len(storedHash) == sha256.Size && subtle.ConstantTimeCompare(storedHash, recipientHash[:]) == 1 {
			response["invite_url"] = a.cfg.PublicBaseURL + "/#/invite/" + recipientToken
		}
	}
	if record.AcceptedAt != nil {
		response["status"] = "accepted"
		response["accepted_at"] = record.AcceptedAt.Format(time.RFC3339)
		response["selected_ideas"] = record.SelectedIdeas
		response["custom_idea"] = record.CustomIdea
		response["selected_slot_index"] = *record.SelectedSlotIndex
		response["recipient_message"] = record.RecipientMessage
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) handleStatusDelete(w http.ResponseWriter, r *http.Request) {
	if !a.validOrigin(w, r) {
		return
	}
	var req tokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	hash, ok := hashToken(req.Token)
	if !ok {
		writeNotFound(w)
		return
	}
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM invites WHERE status_token_hash = ? AND expires_at > ?`, hash[:], a.now().UTC().Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		writeNotFound(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.db.PingContext(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unhealthy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *app) findInvite(ctx context.Context, column, token string) (inviteRecord, error) {
	if column != "recipient_token_hash" && column != "status_token_hash" {
		return inviteRecord{}, sql.ErrNoRows
	}
	hash, ok := hashToken(token)
	if !ok {
		return inviteRecord{}, sql.ErrNoRows
	}
	query := `SELECT id, asker_name, recipient_name, offered_ideas, custom_ideas, sender_message, proposed_slots, created_at, accepted_at, expires_at, selected_ideas, custom_idea, selected_slot_index, recipient_message FROM invites WHERE ` + column + ` = ? AND expires_at > ?`
	var rec inviteRecord
	var ideasJSON, customIdeasJSON, slotsJSON string
	var created, expires int64
	var accepted sql.NullInt64
	var selectedJSON, custom sql.NullString
	var slotIndex sql.NullInt64
	err := a.db.QueryRowContext(ctx, query, hash[:], a.now().UTC().Unix()).Scan(&rec.ID, &rec.AskerName, &rec.RecipientName, &ideasJSON, &customIdeasJSON, &rec.SenderMessage, &slotsJSON, &created, &accepted, &expires, &selectedJSON, &custom, &slotIndex, &rec.RecipientMessage)
	if err != nil {
		return inviteRecord{}, err
	}
	if json.Unmarshal([]byte(ideasJSON), &rec.OfferedIdeas) != nil || json.Unmarshal([]byte(customIdeasJSON), &rec.CustomIdeas) != nil || json.Unmarshal([]byte(slotsJSON), &rec.ProposedSlots) != nil {
		return inviteRecord{}, errors.New("corrupt invite")
	}
	rec.CreatedAt = time.Unix(created, 0).UTC()
	rec.ExpiresAt = time.Unix(expires, 0).UTC()
	if accepted.Valid {
		t := time.Unix(accepted.Int64, 0).UTC()
		rec.AcceptedAt = &t
	}
	if selectedJSON.Valid && json.Unmarshal([]byte(selectedJSON.String), &rec.SelectedIdeas) != nil {
		return inviteRecord{}, errors.New("corrupt response")
	}
	if custom.Valid {
		rec.CustomIdea = custom.String
	}
	if slotIndex.Valid {
		n := int(slotIndex.Int64)
		rec.SelectedSlotIndex = &n
	}
	return rec, nil
}

func validateCreate(req createRequest, now time.Time, validEmojis map[string]struct{}) error {
	if !validName(req.AskerName) || !validName(req.RecipientName) {
		return errors.New("names must be between 1 and 60 characters")
	}
	if len(req.OfferedIdeas) == 0 && len(req.CustomIdeas) == 0 {
		return errors.New("choose at least one valid date idea")
	}
	if len(req.OfferedIdeas) > len(validIdeas) {
		return errors.New("choose only valid date ideas")
	}
	seenIdeas := make(map[string]bool)
	for _, idea := range req.OfferedIdeas {
		if !validIdeas[idea] || seenIdeas[idea] {
			return errors.New("date ideas must be valid and unique")
		}
		seenIdeas[idea] = true
	}
	seenCustomTitles := make(map[string]bool)
	for _, idea := range req.CustomIdeas {
		title := strings.TrimSpace(idea.Title)
		if _, ok := validEmojis[idea.Emoji]; !ok {
			return errors.New("custom date ideas must use one available emoji")
		}
		if !utf8.ValidString(title) || utf8.RuneCountInString(title) < 1 || utf8.RuneCountInString(title) > maxCustomIdeaTitle {
			return errors.New("custom date idea titles must be between 1 and 60 characters")
		}
		key := strings.ToLower(title)
		if seenCustomTitles[key] {
			return errors.New("custom date idea titles must be unique")
		}
		seenCustomTitles[key] = true
	}
	message := strings.TrimSpace(req.SenderMessage)
	if !utf8.ValidString(message) || utf8.RuneCountInString(message) > maxSenderMessage {
		return errors.New("sender message must not exceed 280 characters")
	}
	if len(req.ProposedSlots) < 1 || len(req.ProposedSlots) > 5 {
		return errors.New("provide between one and five times")
	}
	seenSlots := make(map[string]bool)
	for _, raw := range req.ProposedSlots {
		slot, err := time.Parse(time.RFC3339, raw)
		if err != nil || !slot.After(now) || slot.After(now.Add(8*24*time.Hour)) || seenSlots[raw] {
			return errors.New("times must be unique RFC3339 timestamps within the next eight days")
		}
		seenSlots[raw] = true
	}
	return nil
}

func validateAcceptance(req acceptRequest, rec inviteRecord) error {
	custom := strings.TrimSpace(req.CustomIdea)
	if !utf8.ValidString(custom) || utf8.RuneCountInString(custom) > 120 {
		return errors.New("custom idea must not exceed 120 characters")
	}
	message := strings.TrimSpace(req.RecipientMessage)
	if !utf8.ValidString(message) || utf8.RuneCountInString(message) > maxRecipientMessage {
		return errors.New("recipient message must not exceed 280 characters")
	}
	choiceCount := len(req.SelectedIdeas)
	if custom != "" {
		choiceCount++
	}
	if choiceCount != 1 {
		return errors.New("choose exactly one offered or custom date idea")
	}
	seen := make(map[string]bool)
	for _, idea := range req.SelectedIdeas {
		if seen[idea] || !inviteOffersIdea(rec, idea) {
			return errors.New("selected ideas must match the invitation")
		}
		seen[idea] = true
	}
	if req.SelectedSlotIndex == nil || *req.SelectedSlotIndex < 0 || *req.SelectedSlotIndex >= len(rec.ProposedSlots) {
		return errors.New("choose exactly one offered time")
	}
	return nil
}

func normalizeCustomIdeas(inputs []customIdeaInput) []customIdea {
	ideas := make([]customIdea, len(inputs))
	for i, idea := range inputs {
		ideas[i] = customIdea{ID: fmt.Sprintf("custom:%d", i), Emoji: idea.Emoji, Title: strings.TrimSpace(idea.Title)}
	}
	return ideas
}

func inviteOffersIdea(rec inviteRecord, id string) bool {
	if slices.Contains(rec.OfferedIdeas, id) {
		return true
	}
	return slices.ContainsFunc(rec.CustomIdeas, func(idea customIdea) bool { return idea.ID == id })
}

func validName(value string) bool {
	trimmed := strings.TrimSpace(value)
	count := utf8.RuneCountInString(trimmed)
	return utf8.ValidString(trimmed) && count >= 1 && count <= 60
}

func newToken(random io.Reader) (string, [32]byte, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", [32]byte{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	return token, sha256.Sum256([]byte(token)), nil
}

func recipientTokenForStatus(statusToken string) (string, [32]byte, bool) {
	if _, ok := hashToken(statusToken); !ok {
		return "", [32]byte{}, false
	}
	digest := sha256.Sum256([]byte("invite:" + statusToken))
	token := base64.RawURLEncoding.EncodeToString(digest[:16])
	return token, sha256.Sum256([]byte(token)), true
}

func hashToken(token string) ([32]byte, bool) {
	var zero [32]byte
	if len(token) != 22 {
		return zero, false
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) != 16 {
		return zero, false
	}
	canonical := base64.RawURLEncoding.EncodeToString(b)
	if subtle.ConstantTimeCompare([]byte(canonical), []byte(token)) != 1 {
		return zero, false
	}
	return sha256.Sum256([]byte(token)), true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func (a *app) validOrigin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") != a.cfg.PublicBaseURL {
		writeError(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func writeNotFound(w http.ResponseWriter) { writeError(w, http.StatusNotFound, "not found") }

const creationWindow = 24 * time.Hour

func (a *app) allowCreationLocked(ctx context.Context, now time.Time) (bool, int, error) {
	var count int
	var oldest sql.NullInt64
	err := a.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(created_at)
		FROM creation_events WHERE created_at > ?`, now.Add(-creationWindow).Unix()).Scan(&count, &oldest)
	if err != nil {
		return false, 0, err
	}
	if count < a.cfg.GlobalDailyLimit {
		return true, 0, nil
	}
	if !oldest.Valid {
		return false, 0, errors.New("creation limit reached without a creation timestamp")
	}
	remaining := time.Unix(oldest.Int64, 0).UTC().Add(creationWindow).Sub(now)
	retry := int((remaining + time.Second - 1) / time.Second)
	if retry < 1 {
		retry = 1
	}
	return false, retry, nil
}

func writeMutationError(w http.ResponseWriter, err error) {
	if isDatabaseFull(err) {
		writeError(w, http.StatusInsufficientStorage, "invite storage capacity reached")
		return
	}
	writeError(w, http.StatusInternalServerError, "internal error")
}

func isDatabaseFull(err error) bool {
	var sqliteError interface{ Code() int }
	return errors.As(err, &sqliteError) && sqliteError.Code()&0xff == 13
}

func (a *app) cleanup(ctx context.Context) error {
	now := a.now().UTC()
	if _, err := a.db.ExecContext(ctx, `DELETE FROM invites WHERE expires_at <= ?`, now.Unix()); err != nil {
		return err
	}
	_, err := a.db.ExecContext(ctx, `DELETE FROM creation_events WHERE created_at <= ?`, now.Add(-creationWindow).Unix())
	return err
}

func (a *app) runCleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.cleanup(ctx); err != nil { /* deliberately omit request data */
			}
		}
	}
}
