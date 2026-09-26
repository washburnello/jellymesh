package audit

import (
	"context"
	"strings"
	"testing"
	"time"
)

type memorySink struct{ events []Event }

func (sink *memorySink) Record(_ context.Context, event Event) error {
	sink.events = append(sink.events, event)
	return nil
}

// C-OP-1: only allow-listed detail is recorded, and registered secrets are
// scrubbed from every field.
func TestSecretsNeverReachTheSink(t *testing.T) {
	const apiKey = "f3b1c0ffee5ecret4a11"
	redactor := &Redactor{}
	redactor.Register(apiKey, "short")
	sink := &memorySink{}
	log := &Log{Sink: sink, Redactor: redactor, Now: func() time.Time { return time.Unix(0, 0) }}

	err := log.Record(context.Background(), "admin "+apiKey, "sync.failed", "peer "+apiKey, map[string]string{
		"node_id": "walnut",
		"reason":  "GET http://jellyfin/Items?api_key=" + apiKey + " failed",
		"secret":  "the invitation secret",
		"token":   "bearer-token-value",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	event := sink.events[0]
	flat := event.Actor + event.Action + event.Subject
	for _, value := range event.Detail {
		flat += value
	}
	for _, forbidden := range []string{apiKey, "the invitation secret", "bearer-token-value"} {
		if strings.Contains(flat, forbidden) {
			t.Fatalf("recorded event contains %q: %+v", forbidden, event)
		}
	}
	if event.Detail["node_id"] != "walnut" {
		t.Fatal("allow-listed identifiers should be kept")
	}
	if event.Detail["secret"] != Redacted || event.Detail["token"] != Redacted {
		t.Fatal("detail outside the allow-list must be redacted")
	}
	if !strings.Contains(event.Detail["reason"], Redacted) {
		t.Fatal("a registered secret inside an allowed value must be scrubbed")
	}
	if strings.Contains(flat, Redacted+"short") || !strings.Contains(log.Redactor.scrub("short words"), "short") {
		t.Fatal("values too short to be secrets are not registered")
	}
}

func TestANilLogDiscards(t *testing.T) {
	var log *Log
	if err := log.Record(context.Background(), "a", "b", "c", nil); err != nil {
		t.Fatalf("nil log: %v", err)
	}
}
