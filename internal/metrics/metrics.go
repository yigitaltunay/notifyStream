package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	LabelChannel = "channel"
	LabelOutcome = "outcome"
)

var (
	NotificationsSent = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "notifystream_notifications_sent_total",
			Help: "Notifications successfully accepted by webhook (2xx).",
		},
		[]string{LabelChannel},
	)
	NotificationsFailed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "notifystream_notifications_failed_total",
			Help: "Notifications marked failed or sent to DLQ after retries.",
		},
		[]string{LabelChannel, LabelOutcome},
	)
	DeliveryLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "notifystream_delivery_latency_seconds",
			Help:    "Time from worker pickup to webhook response.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 16),
		},
		[]string{LabelChannel},
	)
	RateLimitWait = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "notifystream_rate_limit_wait_seconds",
			Help:    "Time spent waiting on per-channel token bucket before delivery.",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 18),
		},
	)
)
