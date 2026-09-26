package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"jellymesh/internal/audit"
)

func TestAuditRepositoryRoundTrip(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	repo := NewAuditRepository(database)
	first := audit.Event{At: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), Actor: "cedar", Action: "invitation.created", Subject: "i-1", Detail: map[string]string{"invitation_id": "i-1"}}
	second := audit.Event{At: first.At.Add(time.Second), Actor: "local", Action: "peer.blocked", Subject: "maple"}
	for _, event := range []audit.Event{first, second} {
		if err := repo.Record(ctx, event); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	events, err := repo.List(ctx, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("list: %v, %v", events, err)
	}
	if !reflect.DeepEqual(events[0], second) || !reflect.DeepEqual(events[1], first) {
		t.Fatalf("events = %+v, want newest first", events)
	}
}

// C-OP-1: a block decision is audited.
func TestBlockDecisionsAreAudited(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	peers := NewPeerRepository(database)
	peers.SetAudit(&audit.Log{Sink: NewAuditRepository(database)})
	peers.SetBlocked(ctx, "maple", true)
	peers.SetBlocked(ctx, "maple", false)
	events, _ := NewAuditRepository(database).List(ctx, 10)
	if len(events) != 2 || events[1].Action != "peer.blocked" || events[0].Action != "peer.unblocked" || events[0].Subject != "maple" {
		t.Fatalf("events = %+v", events)
	}
}
