package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yaltunay/notifystream/internal/domain"
)

type CreateTemplateRequest struct {
	Name             string          `json:"name"`
	Body             string          `json:"body"`
	Channel          string          `json:"channel"`
	VariablesSchema  json.RawMessage `json:"variables_schema"`
}

type TemplateResponse struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Body            string          `json:"body"`
	Channel         string          `json:"channel"`
	VariablesSchema json.RawMessage `json:"variables_schema,omitempty"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
}

// @Summary Create template
// @Tags templates
// @Accept json
// @Produce json
// @Param body body CreateTemplateRequest true "Body"
// @Success 201 {object} TemplateResponse
// @Failure 400 {object} ErrorResponse
// @Router /v1/templates [post]
func (h *Handler) CreateTemplate(w http.ResponseWriter, r *http.Request) {
	var body CreateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid json body"})
		return
	}
	name := strings.TrimSpace(body.Name)
	b := strings.TrimSpace(body.Body)
	if name == "" || b == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "name and body are required"})
		return
	}
	ch := domain.Channel(body.Channel)
	switch ch {
	case domain.ChannelSMS, domain.ChannelEmail, domain.ChannelPush:
	default:
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid channel"})
		return
	}
	t, err := h.store.CreateTemplate(r.Context(), name, b, ch, body.VariablesSchema)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == pgerrcode.UniqueViolation {
			writeJSON(w, http.StatusConflict, ErrorResponse{Error: "template name already exists"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "could not create template"})
		return
	}
	resp := TemplateResponse{
		ID:        t.ID.String(),
		Name:      t.Name,
		Body:      t.Body,
		Channel:   string(t.Channel),
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: t.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if len(t.VariablesSchema) > 0 {
		resp.VariablesSchema = json.RawMessage(t.VariablesSchema)
	}
	writeJSON(w, http.StatusCreated, resp)
}
