package chat

import (
	"fmt"
	"net/url"
	"strings"
)

// OwnerKey is the full ownership partition of a chat record. Record IDs alone
// are never sufficient identity, because the same record ID can exist in
// another app, mount, auth collection, user or tenant. Every store lookup must
// compare the complete key.
type OwnerKey struct {
	Namespace        string `json:"namespace"`
	Mount            string `json:"mount"`
	AuthCollectionID string `json:"authCollectionId"`
	UserID           string `json:"userId"`
	// Tenant is optional and only set by tenant-aware hosts.
	Tenant string `json:"tenant,omitempty"`
}

// Validate reports whether the required identity parts are present.
func (k OwnerKey) Validate() error {
	var missing []string
	if k.Namespace == "" {
		missing = append(missing, "namespace")
	}
	if k.Mount == "" {
		missing = append(missing, "mount")
	}
	if k.AuthCollectionID == "" {
		missing = append(missing, "authCollectionId")
	}
	if k.UserID == "" {
		missing = append(missing, "userId")
	}
	if len(missing) > 0 {
		return fmt.Errorf("chat: owner key missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// String encodes the key unambiguously for map keys and log correlation.
// Every field is escaped and all five are always present, so distinct keys can
// never produce the same string (for example {"a|b","c"} and {"a","b|c"}).
func (k OwnerKey) String() string {
	parts := [5]string{k.Namespace, k.Mount, k.AuthCollectionID, k.UserID, k.Tenant}
	for i, p := range parts {
		parts[i] = url.QueryEscape(p)
	}
	return strings.Join(parts[:], "|")
}

// Equal reports whether two keys address the same partition.
func (k OwnerKey) Equal(o OwnerKey) bool { return k == o }
