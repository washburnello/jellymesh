// Package audit records what a node did and decided, for its operator
// (design-spec section 13, "audit logs with redacted secrets").
//
// Redaction works in two layers, because either alone fails open. Detail is
// recorded only for keys on an allow-list of identifiers and outcomes, so
// free-form data cannot reach the log under a field name nobody anticipated.
// On top of that, known secret material registered with the Redactor, such as
// the Jellyfin API key, is scrubbed from every field, including actor and
// subject, in case it turns up inside an otherwise permitted value such as an
// error message.
package audit

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Redacted replaces any value that may not be recorded.
const Redacted = "[redacted]"

// allowedDetail are the detail keys whose values may be recorded. They are
// identifiers, counts, and outcomes. Keys that could carry secrets (codes,
// hashes of codes, keys, tokens, URLs with credentials) are absent by design.
var allowedDetail = map[string]bool{
	"group_id":      true,
	"node_id":       true,
	"invitation_id": true,
	"fingerprint":   true,
	"kind":          true,
	"sequence":      true,
	"epoch":         true,
	"status":        true,
	"reason":        true,
	"library_id":    true,
	"count":         true,
	"outcome":       true,
}

// Event is one audit record.
type Event struct {
	At      time.Time
	Actor   string
	Action  string
	Subject string
	Detail  map[string]string
}

// Sink stores audit events; store.AuditRepository satisfies it.
type Sink interface {
	Record(ctx context.Context, event Event) error
}

// Redactor scrubs events before they reach a sink.
type Redactor struct {
	mutex   sync.RWMutex
	secrets []string
}

// minimumSecretLength keeps a short registered value from scrubbing ordinary
// text; nothing Jellymesh treats as a secret is this short.
const minimumSecretLength = 8

// Register adds secret material to scrub wherever it appears.
func (redactor *Redactor) Register(secrets ...string) {
	redactor.mutex.Lock()
	defer redactor.mutex.Unlock()
	for _, secret := range secrets {
		if len(secret) >= minimumSecretLength {
			redactor.secrets = append(redactor.secrets, secret)
		}
	}
	// Longest first, so a secret that contains another is removed whole.
	sort.Slice(redactor.secrets, func(left, right int) bool {
		return len(redactor.secrets[left]) > len(redactor.secrets[right])
	})
}

// Redact returns event with disallowed detail replaced and registered
// secrets scrubbed from every field.
func (redactor *Redactor) Redact(event Event) Event {
	redacted := Event{
		At:      event.At.UTC(),
		Actor:   redactor.scrub(event.Actor),
		Action:  redactor.scrub(event.Action),
		Subject: redactor.scrub(event.Subject),
	}
	if len(event.Detail) > 0 {
		redacted.Detail = make(map[string]string, len(event.Detail))
		for key, value := range event.Detail {
			if allowedDetail[key] {
				redacted.Detail[key] = redactor.scrub(value)
			} else {
				redacted.Detail[key] = Redacted
			}
		}
	}
	return redacted
}

func (redactor *Redactor) scrub(value string) string {
	if redactor == nil {
		return value
	}
	redactor.mutex.RLock()
	defer redactor.mutex.RUnlock()
	for _, secret := range redactor.secrets {
		value = strings.ReplaceAll(value, secret, Redacted)
	}
	return value
}

// Log redacts events and writes them to a sink. A nil Log, or one without a
// sink, discards events, so producers can audit unconditionally.
type Log struct {
	Sink     Sink
	Redactor *Redactor
	Now      func() time.Time
}

// Record audits an action. Failing to audit never fails the action being
// audited; it is reported to the caller, which may log it.
func (log *Log) Record(ctx context.Context, actor string, action string, subject string, detail map[string]string) error {
	if log == nil || log.Sink == nil {
		return nil
	}
	now := time.Now
	if log.Now != nil {
		now = log.Now
	}
	event := Event{At: now(), Actor: actor, Action: action, Subject: subject, Detail: detail}
	if log.Redactor != nil {
		event = log.Redactor.Redact(event)
	} else {
		event = (&Redactor{}).Redact(event)
	}
	return log.Sink.Record(ctx, event)
}
