package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

func init() {
	_ = godotenv.Load()
}

type Config struct {
	DatabaseURL string
	AMQPURL     string
	WebhookURL  string
	HTTPAddr    string
	MetricsAddr string
	RedisURL    string
}

func Load(requireWebhook bool) (Config, error) {
	c := Config{
		DatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")),
		AMQPURL:     strings.TrimSpace(os.Getenv("AMQP_URL")),
		WebhookURL:  strings.TrimSpace(os.Getenv("WEBHOOK_URL")),
		HTTPAddr:    strings.TrimSpace(os.Getenv("HTTP_ADDR")),
		MetricsAddr: strings.TrimSpace(os.Getenv("METRICS_ADDR")),
		RedisURL:    strings.TrimSpace(os.Getenv("REDIS_URL")),
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8080"
	}

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.AMQPURL == "" {
		missing = append(missing, "AMQP_URL")
	}
	if requireWebhook && c.WebhookURL == "" {
		missing = append(missing, "WEBHOOK_URL")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return c, nil
}
