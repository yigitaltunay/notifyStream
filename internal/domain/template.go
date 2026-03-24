package domain

import (
	"time"

	"github.com/google/uuid"
)

type Template struct {
	ID               uuid.UUID
	Name             string
	Body             string
	Channel          Channel
	VariablesSchema  []byte
	CreatedAt        time.Time
	UpdatedAt        time.Time
}
