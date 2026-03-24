package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type WSHub struct {
	mu        sync.RWMutex
	byNotif   map[uuid.UUID]map[*websocket.Conn]struct{}
	byBatch   map[uuid.UUID]map[*websocket.Conn]struct{}
	broadcast chan []byte
}

func NewWSHub() *WSHub {
	h := &WSHub{
		byNotif:   make(map[uuid.UUID]map[*websocket.Conn]struct{}),
		byBatch:   make(map[uuid.UUID]map[*websocket.Conn]struct{}),
		broadcast: make(chan []byte, 256),
	}
	go h.run()
	return h
}

func (h *WSHub) run() {
	for msg := range h.broadcast {
		var ev struct {
			NotificationID string  `json:"notification_id"`
			BatchID        *string `json:"batch_id"`
		}
		if err := json.Unmarshal(msg, &ev); err != nil {
			slog.Warn("ws status decode", "error", err)
			continue
		}
		nid, err := uuid.Parse(ev.NotificationID)
		if err != nil {
			continue
		}
		h.writeToNotif(nid, msg)
		if ev.BatchID != nil {
			if bid, err := uuid.Parse(*ev.BatchID); err == nil {
				h.writeToBatch(bid, msg)
			}
		}
	}
}

func (h *WSHub) writeToNotif(id uuid.UUID, msg []byte) {
	h.mu.RLock()
	conns := h.byNotif[id]
	h.mu.RUnlock()
	for c := range conns {
		_ = c.WriteMessage(websocket.TextMessage, msg)
	}
}

func (h *WSHub) writeToBatch(id uuid.UUID, msg []byte) {
	h.mu.RLock()
	conns := h.byBatch[id]
	h.mu.RUnlock()
	for c := range conns {
		_ = c.WriteMessage(websocket.TextMessage, msg)
	}
}

func (h *WSHub) addNotif(id uuid.UUID, c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byNotif[id] == nil {
		h.byNotif[id] = make(map[*websocket.Conn]struct{})
	}
	h.byNotif[id][c] = struct{}{}
}

func (h *WSHub) addBatch(id uuid.UUID, c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byBatch[id] == nil {
		h.byBatch[id] = make(map[*websocket.Conn]struct{})
	}
	h.byBatch[id][c] = struct{}{}
}

func (h *WSHub) removeConn(c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for nid, m := range h.byNotif {
		delete(m, c)
		if len(m) == 0 {
			delete(h.byNotif, nid)
		}
	}
	for bid, m := range h.byBatch {
		delete(m, c)
		if len(m) == 0 {
			delete(h.byBatch, bid)
		}
	}
}

func (h *WSHub) DispatchStatus(msg []byte) {
	select {
	case h.broadcast <- msg:
	default:
		slog.Warn("ws broadcast dropped")
	}
}

// @Summary WebSocket status stream
// @Description Subscribe with query notification_id and/or batch_id. Server pushes JSON status events.
// @Tags websocket
// @Param notification_id query string false "Notification UUID"
// @Param batch_id query string false "Batch UUID"
// @Router /v1/ws [get]
func (h *WSHub) HandleWS(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var nid uuid.UUID
	var bid uuid.UUID
	var hasN, hasB bool
	if s := q.Get("notification_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			http.Error(w, "invalid notification_id", http.StatusBadRequest)
			return
		}
		nid = id
		hasN = true
	}
	if s := q.Get("batch_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			http.Error(w, "invalid batch_id", http.StatusBadRequest)
			return
		}
		bid = id
		hasB = true
	}
	if !hasN && !hasB {
		http.Error(w, "notification_id or batch_id required", http.StatusBadRequest)
		return
	}
	c, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if hasN {
		h.addNotif(nid, c)
	}
	if hasB {
		h.addBatch(bid, c)
	}
	defer func() {
		h.removeConn(c)
		_ = c.Close()
	}()
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}
