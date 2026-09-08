package config

import (
	"bytes"
	"sync"
)

// Credential is the part of a Backend that may be rotated while the driver
// runs. Everything else in a Backend is identity — which appliance, which pool,
// which dataset — and changing it needs a restart; see CheckReloadable.
type Credential struct {
	Username           string
	APIKey             string
	CACert             []byte
	InsecureSkipVerify bool
}

// Equal reports whether two credentials are the same secret material.
func (c Credential) Equal(o Credential) bool {
	return c.Username == o.Username &&
		c.APIKey == o.APIKey &&
		c.InsecureSkipVerify == o.InsecureSkipVerify &&
		bytes.Equal(c.CACert, o.CACert)
}

// Credentials returns b's credential fields.
func (b Backend) Credentials() Credential {
	return Credential{
		Username:           b.Username,
		APIKey:             b.APIKey,
		CACert:             b.CACert,
		InsecureSkipVerify: b.InsecureSkipVerify,
	}
}

// The process-wide store of rotated credentials, keyed by backend name.
//
// This exists because a Backend travels through the driver BY VALUE: the
// Config read at startup is handed to the backend registry, which copies each
// Backend into every client it dials. A key rotated at 03:00 would otherwise
// only reach connections that already exist — and the one case that matters
// most is the opposite, an appliance that is unreachable while the key is
// rotated, whose next dial would present the old key. Presenting a stale key is
// not a harmless retry on TrueNAS: it is how an API key gets revoked.
//
// So every dial resolves its credential through here first. The mutex makes
// that safe against the registry's unsynchronised map of Backends, which this
// deliberately never writes to.
var (
	credMu sync.RWMutex
	creds  = map[string]Credential{}
)

// SetCredential records the credential a named backend should use from now on.
// It affects every subsequent dial, including dials made from a copy of the
// Backend that was loaded at startup.
func SetCredential(name string, c Credential) {
	credMu.Lock()
	defer credMu.Unlock()
	creds[name] = c
}

// ForgetCredential drops a recorded credential, so the Backend's own fields
// apply again. It exists for tests; the driver never un-rotates a key.
func ForgetCredential(name string) {
	credMu.Lock()
	defer credMu.Unlock()
	delete(creds, name)
}

// Current returns b with the most recently recorded credential for its name
// applied. When nothing has been recorded it returns b unchanged, so code that
// never reloads behaves exactly as it did before.
func (b Backend) Current() Backend {
	credMu.RLock()
	c, ok := creds[b.Name]
	credMu.RUnlock()
	if !ok {
		return b
	}
	b.Username = c.Username
	b.APIKey = c.APIKey
	b.CACert = c.CACert
	b.InsecureSkipVerify = c.InsecureSkipVerify
	return b
}
