package resources

import (
	"context"
	"encoding/json"
	"time"
)

const OutboxChannel = "resources_outbox"

// OutboxReader is a trusted relay port. The relay serializes each source/subject
// and confirms only after every subscribed consumer durably accepts the event.
// Exclude up to 1000 subjects currently backing off or awaiting manual handling.
type OutboxReader interface {
	PendingOutbox(context.Context, int, []string) ([]Envelope, error)
	ConfirmOutbox(context.Context, string) error
}

// Envelope carries either a discovery document or the minimal business revision.
// Consumers select a payload type using Type; no private persistence model is sent.
type Envelope struct {
	SpecVersion string          `json:"specversion"`
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	Type        string          `json:"type"`
	Subject     string          `json:"subject"`
	Time        time.Time       `json:"time"`
	Data        json.RawMessage `json:"data"`
}
