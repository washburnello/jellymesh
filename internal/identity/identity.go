package identity

import (
	"errors"
	"fmt"
	"strings"
)

var ErrInvalidIdentity = errors.New("work identity requires a media type, provider, and provider ID")

type WorkIdentity struct {
	Type     string
	Provider string
	ID       string
}

func NewWorkIdentity(mediaType, provider, id string) (WorkIdentity, error) {
	identity := WorkIdentity{
		Type:     strings.TrimSpace(mediaType),
		Provider: strings.TrimSpace(provider),
		ID:       strings.TrimSpace(id),
	}
	if identity.Type == "" || identity.Provider == "" || identity.ID == "" {
		return WorkIdentity{}, ErrInvalidIdentity
	}
	return identity, nil
}

func (identity WorkIdentity) Key() string {
	return strings.ToLower(strings.Join([]string{identity.Type, identity.Provider, identity.ID}, ":"))
}

func (identity WorkIdentity) StronglyMatches(other WorkIdentity) bool {
	if identity.Type == "" || identity.Provider == "" || identity.ID == "" {
		return false
	}
	return identity.Key() == other.Key()
}

func (identity WorkIdentity) String() string {
	return fmt.Sprintf("%s/%s/%s", identity.Type, identity.Provider, identity.ID)
}
