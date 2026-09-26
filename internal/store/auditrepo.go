package store

import (
	"context"
	"encoding/json"
	"fmt"

	"jellymesh/internal/audit"
)

// AuditRepository stores audit events. It stores what it is given: redaction
// happens in audit.Log before an event reaches it.
type AuditRepository struct {
	database *DB
}

func NewAuditRepository(database *DB) *AuditRepository {
	return &AuditRepository{database: database}
}

// Record satisfies audit.Sink.
func (repo *AuditRepository) Record(ctx context.Context, event audit.Event) error {
	detail := ""
	if len(event.Detail) > 0 {
		encoded, err := json.Marshal(event.Detail)
		if err != nil {
			return fmt.Errorf("encode audit detail: %w", err)
		}
		detail = string(encoded)
	}
	if _, err := repo.database.SQL().ExecContext(ctx, `
		INSERT INTO audit_events (occurred_at, actor, action, subject, detail) VALUES (?, ?, ?, ?, ?)`,
		FormatTime(event.At), event.Actor, event.Action, event.Subject, detail,
	); err != nil {
		return fmt.Errorf("record audit event: %w", err)
	}
	return nil
}

// List returns up to limit events, newest first.
func (repo *AuditRepository) List(ctx context.Context, limit int) ([]audit.Event, error) {
	rows, err := repo.database.SQL().QueryContext(ctx, `
		SELECT occurred_at, actor, action, subject, detail FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	var events []audit.Event
	for rows.Next() {
		var event audit.Event
		var at, detail string
		if err := rows.Scan(&at, &event.Actor, &event.Action, &event.Subject, &detail); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		if event.At, err = ParseTime(at); err != nil {
			return nil, fmt.Errorf("parse audit time: %w", err)
		}
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &event.Detail); err != nil {
				return nil, fmt.Errorf("decode audit detail: %w", err)
			}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
