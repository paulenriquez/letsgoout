package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
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
	validCustomIdeaEmojis = map[string]bool{
		"🍕": true, "🍜": true, "☕": true, "🥂": true, "🥩": true, "🏋️": true,
		"👟": true, "🏃": true, "🎁": true, "🎬": true, "🎨": true, "🎳": true,
		"🎤": true, "🎮": true, "🧺": true, "🌅": true, "🏖️": true, "🏛️": true,
		"📚": true, "🍦": true, "🧋": true, "🍣": true, "🌮": true, "🚲": true,
		"🧗": true, "🎭": true, "🎡": true, "🌿": true, "✨": true,
	}
)

type app struct {
	db       *sql.DB
	cfg      config
	static   fs.FS
	now      func() time.Time
	random   io.Reader
	createMu sync.Mutex
	limits   *memoryLimiter
}

func newApp(db *sql.DB, cfg config, static fs.FS, now func() time.Time, random io.Reader) *app {
	return &app{db: db, cfg: cfg, static: static, now: now, random: random, limits: newMemoryLimiter()}
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
		files.ServeHTTP(w, r)
	})
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
	var ipHash []byte
	if !a.cfg.DisableRateLimits {
		ipHash = a.ipHash(clientIP(r))
		if ok, retry := a.allowCreation(r.Context(), ipHash); !ok {
			w.Header().Set("Retry-After", fmt.Sprint(retry))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
	}
	var req createRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	now := a.now().UTC()
	if err := validateCreate(req, now); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	recipientToken, recipientHash, err := newToken(a.random)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	statusToken, statusHash, err := newToken(a.random)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	ideasJSON, _ := json.Marshal(req.OfferedIdeas)
	customIdeasJSON, _ := json.Marshal(normalizeCustomIdeas(req.CustomIdeas))
	slotsJSON, _ := json.Marshal(req.ProposedSlots)
	expires := now.Add(initialLifetime)

	a.createMu.Lock()
	defer a.createMu.Unlock()
	if !a.cfg.DisableRateLimits {
		if ok, retry := a.allowCreationLocked(r.Context(), ipHash, now); !ok {
			w.Header().Set("Retry-After", fmt.Sprint(retry))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO invites (
			recipient_token_hash, status_token_hash, asker_name, recipient_name,
			offered_ideas, custom_ideas, sender_message, proposed_slots, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, recipientHash[:], statusHash[:], strings.TrimSpace(req.AskerName), strings.TrimSpace(req.RecipientName), ideasJSON, customIdeasJSON, strings.TrimSpace(req.SenderMessage), slotsJSON, now.Unix(), expires.Unix())
	}
	if err == nil && !a.cfg.DisableRateLimits {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO creation_events(ip_hash, created_at) VALUES (?, ?)`, ipHash, now.Unix())
	}
	if err != nil {
		if tx != nil {
			tx.Rollback()
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
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
	if !a.allowMemory(w, r, "view", a.cfg.ViewMinuteLimit) {
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
	if !a.allowMemory(w, r, "accept", a.cfg.AcceptMinuteLimit) {
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
	if !a.allowMemory(w, r, "view", a.cfg.ViewMinuteLimit) {
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
	if !a.allowMemory(w, r, "view", a.cfg.ViewMinuteLimit) {
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

func validateCreate(req createRequest, now time.Time) error {
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
		if !validCustomIdeaEmojis[idea.Emoji] {
			return errors.New("custom date ideas must use an available emoji")
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
	if len(req.SelectedIdeas) == 0 && custom == "" {
		return errors.New("choose at least one offered or custom vibe")
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

func (a *app) ipHash(ip netip.Addr) []byte {
	mac := hmac.New(sha256.New, a.cfg.RateLimitHMACKey)
	mac.Write([]byte(ip.Unmap().String()))
	return mac.Sum(nil)
}

func clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	immediate, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.IPv4Unspecified()
	}
	immediate = immediate.Unmap()
	if !isTrustedProxy(immediate) {
		return immediate
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	var parsed []netip.Addr
	for _, part := range parts {
		if ip, err := netip.ParseAddr(strings.TrimSpace(part)); err == nil {
			parsed = append(parsed, ip.Unmap())
		}
	}
	for i := len(parsed) - 1; i >= 0; i-- {
		if !isTrustedProxy(parsed[i]) {
			return parsed[i]
		}
	}
	if len(parsed) > 0 {
		return parsed[0]
	}
	return immediate
}

func isTrustedProxy(ip netip.Addr) bool { return ip.IsValid() && (ip.IsPrivate() || ip.IsLoopback()) }

type memoryLimiter struct {
	mu     sync.Mutex
	events map[string][]time.Time
}

func newMemoryLimiter() *memoryLimiter { return &memoryLimiter{events: make(map[string][]time.Time)} }

func (l *memoryLimiter) allow(key string, now time.Time, limit int, window time.Duration) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-window)
	events := l.events[key]
	first := 0
	for first < len(events) && !events[first].After(cutoff) {
		first++
	}
	events = events[first:]
	if len(events) >= limit {
		remaining := events[0].Add(window).Sub(now)
		retry := int((remaining + time.Second - 1) / time.Second)
		if retry < 1 {
			retry = 1
		}
		l.events[key] = events
		return false, retry
	}
	l.events[key] = append(events, now)
	return true, 0
}

func (l *memoryLimiter) cleanup(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	for key, events := range l.events {
		if len(events) == 0 || !events[len(events)-1].After(cutoff) {
			delete(l.events, key)
		}
	}
}

func (a *app) allowMemory(w http.ResponseWriter, r *http.Request, bucket string, limit int) bool {
	if a.cfg.DisableRateLimits {
		return true
	}
	key := bucket + ":" + base64.RawURLEncoding.EncodeToString(a.ipHash(clientIP(r)))
	ok, retry := a.limits.allow(key, a.now().UTC(), limit, time.Minute)
	if !ok {
		w.Header().Set("Retry-After", fmt.Sprint(retry))
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
	}
	return ok
}

// allowCreation performs a cheap unlocked pre-check. The authoritative check and
// insert are serialized with createMu so concurrent creates cannot exceed quotas.
func (a *app) allowCreation(ctx context.Context, ipHash []byte) (bool, int) {
	a.createMu.Lock()
	defer a.createMu.Unlock()
	return a.allowCreationLocked(ctx, ipHash, a.now().UTC())
}

func (a *app) allowCreationLocked(ctx context.Context, ipHash []byte, now time.Time) (bool, int) {
	var hourly, daily, global int
	err := a.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN ip_hash = ? AND created_at > ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ip_hash = ? AND created_at > ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN created_at > ? THEN 1 ELSE 0 END), 0)
		FROM creation_events WHERE created_at > ?`, ipHash, now.Add(-time.Hour).Unix(), ipHash, now.Add(-24*time.Hour).Unix(), now.Add(-24*time.Hour).Unix(), now.Add(-48*time.Hour).Unix()).Scan(&hourly, &daily, &global)
	if err != nil {
		return false, 1
	}
	if hourly >= a.cfg.CreateHourlyLimit {
		return false, 3600
	}
	if daily >= a.cfg.CreateDailyLimit || global >= a.cfg.GlobalDailyLimit {
		return false, 86400
	}
	return true, 0
}

func (a *app) cleanup(ctx context.Context) error {
	now := a.now().UTC()
	a.limits.cleanup(now)
	if _, err := a.db.ExecContext(ctx, `DELETE FROM invites WHERE expires_at <= ?`, now.Unix()); err != nil {
		return err
	}
	_, err := a.db.ExecContext(ctx, `DELETE FROM creation_events WHERE created_at <= ?`, now.Add(-48*time.Hour).Unix())
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
