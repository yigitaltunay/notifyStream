package ratelimit

import (
	"context"
	"testing"

	"github.com/yaltunay/notifystream/internal/domain"
)

func TestMemoryWaiter_Wait(t *testing.T) {
	t.Parallel()
	w, cleanup, err := NewChannelWaiter("", 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx := context.Background()
	if err := w.Wait(ctx, domain.ChannelSMS); err != nil {
		t.Fatal(err)
	}
}
