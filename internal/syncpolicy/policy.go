package syncpolicy

import (
	"errors"
	"time"
)

const (
	DefaultHealthInterval          = 5 * time.Minute
	DefaultCatalogInterval         = time.Hour
	DefaultOfflineCatalogRetention = 15 * 24 * time.Hour
	DefaultMaximumBackoff          = time.Hour
	DefaultCatalogPageLimit        = 5000
)

var (
	ErrInvalidHealthInterval          = errors.New("health interval must be positive")
	ErrInvalidCatalogInterval         = errors.New("catalog interval must be positive")
	ErrInvalidOfflineCatalogRetention = errors.New("offline catalog retention must be positive")
	ErrInvalidBackoff                 = errors.New("maximum backoff must be positive")
	ErrInvalidPageLimit               = errors.New("catalog page limit must be positive")
)

type Policy struct {
	HealthInterval          time.Duration
	CatalogInterval         time.Duration
	OfflineCatalogRetention time.Duration
	MaximumBackoff          time.Duration
	CatalogPageLimit        int
}

func DefaultPolicy() Policy {
	return Policy{
		HealthInterval:          DefaultHealthInterval,
		CatalogInterval:         DefaultCatalogInterval,
		OfflineCatalogRetention: DefaultOfflineCatalogRetention,
		MaximumBackoff:          DefaultMaximumBackoff,
		CatalogPageLimit:        DefaultCatalogPageLimit,
	}
}

func (policy Policy) Validate() error {
	if policy.HealthInterval <= 0 {
		return ErrInvalidHealthInterval
	}
	if policy.CatalogInterval <= 0 {
		return ErrInvalidCatalogInterval
	}
	if policy.OfflineCatalogRetention <= 0 {
		return ErrInvalidOfflineCatalogRetention
	}
	if policy.MaximumBackoff <= 0 {
		return ErrInvalidBackoff
	}
	if policy.CatalogPageLimit <= 0 {
		return ErrInvalidPageLimit
	}
	return nil
}

// Backoff returns the un-jittered exponential delay for an attempt. Callers
// that schedule real network work must use BackoffWithJitter instead; the
// design requires per-peer jitter so that a group-wide outage does not produce
// a synchronized retry burst when every node recovers at once.
func (policy Policy) Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	backoff := time.Second
	for index := 0; index < attempt && backoff < policy.MaximumBackoff; index++ {
		backoff *= 2
	}
	if backoff > policy.MaximumBackoff {
		return policy.MaximumBackoff
	}
	return backoff
}

// DefaultJitterFraction is the portion of a backoff interval that is randomized.
const DefaultJitterFraction = 0.2

// BackoffWithJitter applies full +/- DefaultJitterFraction jitter to the
// exponential delay. The random source is supplied by the caller so that the
// scheduler remains deterministic under test.
func (policy Policy) BackoffWithJitter(attempt int, random func() float64) time.Duration {
	base := policy.Backoff(attempt)
	if random == nil {
		return base
	}
	// random() is expected in [0,1); map it to [-1,1).
	offset := (random()*2 - 1) * DefaultJitterFraction
	jittered := time.Duration(float64(base) * (1 + offset))
	if jittered < time.Second {
		jittered = time.Second
	}
	if jittered > policy.MaximumBackoff {
		jittered = policy.MaximumBackoff
	}
	return jittered
}
