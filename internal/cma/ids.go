package cma

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// ID prefixes mirror the Anthropic resource conventions.
const (
	PrefixAgent       = "agent"
	PrefixEnvironment = "env"
	PrefixSession     = "sesn"
	PrefixEvent       = "sevt"
)

var enc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewID returns a prefixed, collision-resistant id, e.g. "agent_a1b2c3...".
func NewID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + "_" + strings.ToLower(enc.EncodeToString(b))
}
