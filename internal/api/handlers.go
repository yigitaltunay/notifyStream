package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yaltunay/notifystream/internal/amqp"
	"github.com/yaltunay/notifystream/internal/db"
	"github.com/yaltunay/notifystream/internal/domain"
)

type Handler struct {
	store *db.Store
	bus   *amqp.Client
}

func NewHandler(store *db.Store, bus *amqp.Client) *Handler {
	return &Handler{store: store, bus: bus}
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type FieldError struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}

type ValidationErrorResponse struct {
	Errors []FieldError `json:"errors"`
}

type CreateNotificationItem struct {
	Recipient      string          `json:"recipient"`
	Channel        string          `json:"channel"`
	Content        *string         `json:"content"`
	TemplateID     *uuid.UUID      `json:"template_id"`
	Payload        json.RawMessage `json:"payload"`
	Priority       string          `json:"priority"`
	IdempotencyKey *string         `json:"idempotency_key"`
	ScheduledAt    *time.Time      `json:"scheduled_at"`
}

type CreateNotificationsRequest struct {
	BatchMetadata json.RawMessage          `json:"batch_metadata"`
	Notifications []CreateNotificationItem `json:"notifications"`
}

type CreateNotificationsResponse struct {
	BatchID        *uuid.UUID             `json:"batch_id"`
	Notifications  []NotificationResponse `json:"notifications"`
	IdempotentHits int                    `json:"idempotent_hits"`
}

type NotificationResponse struct {
	ID                uuid.UUID       `json:"id"`
	BatchID           *uuid.UUID      `json:"batch_id,omitempty"`
	Recipient         string          `json:"recipient"`
	Channel           string          `json:"channel"`
	Content           *string         `json:"content,omitempty"`
	TemplateID        *uuid.UUID      `json:"template_id,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	Priority          string          `json:"priority"`
	Status            string          `json:"status"`
	IdempotencyKey    *string         `json:"idempotency_key,omitempty"`
	ProviderMessageID *string         `json:"provider_message_id,omitempty"`
	ScheduledAt       *time.Time      `json:"scheduled_at,omitempty"`
	CorrelationID     *string         `json:"correlation_id,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	Inserted          bool            `json:"inserted"`
}

type NotificationListResponse struct {
	Notifications []NotificationResponse `json:"notifications"`
	NextCursor    string                 `json:"next_cursor"`
}

type BatchNotificationsResponse struct {
	Notifications []NotificationResponse `json:"notifications"`
}

func toNotificationJSON(n domain.Notification, inserted bool) NotificationResponse {
	out := NotificationResponse{
		ID:                n.ID,
		BatchID:           n.BatchID,
		Recipient:         n.Recipient,
		Channel:           string(n.Channel),
		Content:           n.Content,
		TemplateID:        n.TemplateID,
		Priority:          string(n.Priority),
		Status:            string(n.Status),
		IdempotencyKey:    n.IdempotencyKey,
		ProviderMessageID: n.ProviderMessageID,
		ScheduledAt:       n.ScheduledAt,
		CorrelationID:     n.CorrelationID,
		CreatedAt:         n.CreatedAt,
		UpdatedAt:         n.UpdatedAt,
		Inserted:          inserted,
	}
	if len(n.Payload) > 0 {
		out.Payload = json.RawMessage(n.Payload)
	}
	return out
}

// @Summary Create notifications
// @Tags notifications
// @Accept json
// @Produce json
// @Param X-Request-ID header string false "Correlation ID"
// @Param body body CreateNotificationsRequest true "Body"
// @Success 201 {object} CreateNotificationsResponse
// @Failure 400 {object} ErrorResponse
// @Router /v1/notifications [post]
func (h *Handler) CreateNotifications(w http.ResponseWriter, r *http.Request) {
	var body CreateNotificationsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid json body"})
		return
	}
	if len(body.Notifications) == 0 {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "notifications must not be empty"})
		return
	}
	if len(body.Notifications) > 1000 {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "at most 1000 notifications per request"})
		return
	}
	var valErrs []FieldError
	items := make([]domain.CreateItem, len(body.Notifications))
	for i, it := range body.Notifications {
		items[i] = domain.CreateItem{
			Recipient:      it.Recipient,
			Channel:        domain.Channel(it.Channel),
			Content:        it.Content,
			TemplateID:     it.TemplateID,
			Payload:        it.Payload,
			Priority:       domain.Priority(it.Priority),
			IdempotencyKey: it.IdempotencyKey,
			ScheduledAt:    it.ScheduledAt,
		}
		if err := domain.ValidateCreateItem(items[i]); err != nil {
			valErrs = append(valErrs, FieldError{Index: i, Message: err.Error()})
		}
	}
	if len(valErrs) > 0 {
		writeJSON(w, http.StatusBadRequest, ValidationErrorResponse{Errors: valErrs})
		return
	}

	corr := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if corr == "" {
		corr = middleware.GetReqID(r.Context())
	}
	if corr == "" {
		corr = uuid.NewString()
	}
	corrPtr := corr

	ctx := r.Context()
	tx, err := h.store.Pool().Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "database error"})
		return
	}
	defer tx.Rollback(ctx)

	var batchID *uuid.UUID
	if len(items) > 1 {
		bid, err := h.store.CreateBatch(ctx, tx, body.BatchMetadata, len(items))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "could not create batch"})
			return
		}
		batchID = &bid
	}

	results := make([]NotificationResponse, 0, len(items))
	idemHits := 0
	for _, item := range items {
		n, inserted, err := h.store.InsertNotification(ctx, tx, batchID, item, &corrPtr)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "could not persist notification"})
			return
		}
		if !inserted {
			idemHits++
		}
		results = append(results, toNotificationJSON(n, inserted))
	}
	if err := tx.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "could not commit"})
		return
	}

	for i := range results {
		if !results[i].Inserted {
			continue
		}
		id := results[i].ID
		n, err := h.store.GetByID(ctx, id)
		if err != nil {
			continue
		}
		pubErr := h.bus.PublishNotification(ctx, n)
		tx2, err := h.store.Pool().Begin(ctx)
		if err != nil {
			continue
		}
		if pubErr != nil {
			_ = h.store.EnqueueOutbox(ctx, tx2, id, map[string]any{
				"error": pubErr.Error(),
			})
			_ = tx2.Commit(ctx)
			continue
		}
		if err := h.store.MarkQueued(ctx, tx2, id); err != nil {
			_ = tx2.Rollback(ctx)
			continue
		}
		_ = tx2.Commit(ctx)
		results[i].Status = string(domain.StatusQueued)
	}

	writeJSON(w, http.StatusCreated, CreateNotificationsResponse{
		BatchID:        batchID,
		Notifications:  results,
		IdempotentHits: idemHits,
	})
}

// @Summary Get notification
// @Tags notifications
// @Produce json
// @Param id path string true "Notification ID (UUID)"
// @Success 200 {object} NotificationResponse
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /v1/notifications/{id} [get]
func (h *Handler) GetNotification(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid id"})
		return
	}
	n, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "database error"})
		return
	}
	writeJSON(w, http.StatusOK, toNotificationJSON(n, false))
}

// @Summary List notifications
// @Tags notifications
// @Produce json
// @Param status query string false "Status" Enums(pending, queued, sending, delivered, failed, cancelled)
// @Param channel query string false "Channel" Enums(sms, email, push)
// @Param from query string false "RFC3339"
// @Param to query string false "RFC3339"
// @Param limit query int false "1-100" default(20)
// @Param cursor query string false "Pagination cursor"
// @Success 200 {object} NotificationListResponse
// @Failure 400 {object} ErrorResponse
// @Router /v1/notifications [get]
func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var st *domain.Status
	if s := strings.TrimSpace(q.Get("status")); s != "" {
		v := domain.Status(s)
		st = &v
	}
	var ch *domain.Channel
	if s := strings.TrimSpace(q.Get("channel")); s != "" {
		v := domain.Channel(s)
		ch = &v
	}
	var from, to *time.Time
	if s := strings.TrimSpace(q.Get("from")); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid from (RFC3339)"})
			return
		}
		from = &t
	}
	if s := strings.TrimSpace(q.Get("to")); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid to (RFC3339)"})
			return
		}
		to = &t
	}
	limit := 20
	if s := strings.TrimSpace(q.Get("limit")); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid limit"})
			return
		}
		limit = v
	}
	var cAt *time.Time
	var cID *uuid.UUID
	if cur := strings.TrimSpace(q.Get("cursor")); cur != "" {
		at, id, err := decodeCursor(cur)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid cursor"})
			return
		}
		cAt = &at
		cID = &id
	}
	rows, err := h.store.List(r.Context(), db.ListParams{
		Status:   st,
		Channel:  ch,
		From:     from,
		To:       to,
		CursorAt: cAt,
		CursorID: cID,
		Limit:    limit,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "database error"})
		return
	}
	out := make([]NotificationResponse, 0, len(rows))
	for _, n := range rows {
		out = append(out, toNotificationJSON(n, false))
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	writeJSON(w, http.StatusOK, NotificationListResponse{
		Notifications: out,
		NextCursor:    next,
	})
}

// @Summary List batch notifications
// @Tags batches
// @Produce json
// @Param batchId path string true "Batch ID (UUID)"
// @Success 200 {object} BatchNotificationsResponse
// @Failure 400 {object} ErrorResponse
// @Router /v1/batches/{batchId}/notifications [get]
func (h *Handler) ListBatchNotifications(w http.ResponseWriter, r *http.Request) {
	batchID, err := uuid.Parse(chi.URLParam(r, "batchId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid batch id"})
		return
	}
	rows, err := h.store.ListByBatchID(r.Context(), batchID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "database error"})
		return
	}
	out := make([]NotificationResponse, 0, len(rows))
	for _, n := range rows {
		out = append(out, toNotificationJSON(n, false))
	}
	writeJSON(w, http.StatusOK, BatchNotificationsResponse{Notifications: out})
}

// @Summary Cancel notification
// @Tags notifications
// @Produce json
// @Param id path string true "Notification ID (UUID)"
// @Success 200 {object} NotificationResponse
// @Failure 400 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Router /v1/notifications/{id}/cancel [post]
func (h *Handler) CancelNotification(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid id"})
		return
	}
	n, err := h.store.Cancel(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusConflict, ErrorResponse{Error: "notification cannot be cancelled in current state"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "database error"})
		return
	}
	writeJSON(w, http.StatusOK, toNotificationJSON(n, false))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func encodeCursor(t time.Time, id uuid.UUID) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, fmt.Errorf("bad cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return t, id, nil
}
