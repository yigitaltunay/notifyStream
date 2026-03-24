package delivery

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yaltunay/notifystream/internal/amqp"
)

func TestWebhook_Post_2xx_withMessageID(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		if len(b) == 0 {
			t.Error("empty body")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messageId":"provider-msg-1"}`))
	}))
	defer srv.Close()

	w := NewWebhook(srv.URL, nil)
	msg, err := w.Post(context.Background(), amqp.Envelope{
		Recipient: "to@example.com",
		Channel:   "email",
		Priority:  "normal",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if msg != "provider-msg-1" {
		t.Errorf("messageID %q", msg)
	}
}

func TestWebhook_Post_2xx_emptyMessageID(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	w := NewWebhook(srv.URL, nil)
	msg, err := w.Post(context.Background(), amqp.Envelope{
		Recipient: "x",
		Channel:   "sms",
		Priority:  "low",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if msg != "" {
		t.Errorf("expected empty messageID, got %q", msg)
	}
}

func TestWebhook_Post_500_transient(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream", http.StatusInternalServerError)
	}))
	defer srv.Close()

	w := NewWebhook(srv.URL, nil)
	_, err := w.Post(context.Background(), amqp.Envelope{
		Recipient: "x",
		Channel:   "sms",
		Priority:  "normal",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsTransient(err) {
		t.Errorf("expected transient, got %#v", err)
	}
}

func TestWebhook_Post_400_permanent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	w := NewWebhook(srv.URL, nil)
	_, err := w.Post(context.Background(), amqp.Envelope{
		Recipient: "x",
		Channel:   "sms",
		Priority:  "normal",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsPermanent(err) {
		t.Errorf("expected permanent, got %#v", err)
	}
}
