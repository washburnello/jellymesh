package limits

import "errors"

const DefaultConcurrentRemoteTranscodes = 1

var ErrInvalidConcurrentTranscodes = errors.New("concurrent remote transcode limit must be positive")

type Policy struct {
	ConcurrentRemoteTranscodes int
}

func DefaultPolicy() Policy {
	return Policy{ConcurrentRemoteTranscodes: DefaultConcurrentRemoteTranscodes}
}

func (policy Policy) Validate() error {
	if policy.ConcurrentRemoteTranscodes <= 0 {
		return ErrInvalidConcurrentTranscodes
	}
	return nil
}

func (policy Policy) CanStartRemoteTranscode(active int) bool {
	return policy.Validate() == nil && active < policy.ConcurrentRemoteTranscodes
}
