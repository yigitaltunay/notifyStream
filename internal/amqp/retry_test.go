package amqp

import (
	"testing"

	amqp091 "github.com/rabbitmq/amqp091-go"
)

func TestRetryCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		h    amqp091.Table
		want int
	}{
		{"nil", nil, 0},
		{"empty", amqp091.Table{}, 0},
		{"missing", amqp091.Table{"other": int32(3)}, 0},
		{"int32", amqp091.Table{HeaderRetry: int32(2)}, 2},
		{"int64", amqp091.Table{HeaderRetry: int64(5)}, 5},
		{"int", amqp091.Table{HeaderRetry: 7}, 7},
		{"byte", amqp091.Table{HeaderRetry: byte(1)}, 1},
		{"wrong type", amqp091.Table{HeaderRetry: "nope"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RetryCount(tt.h); got != tt.want {
				t.Errorf("RetryCount() = %d, want %d", got, tt.want)
			}
		})
	}
}
