package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is the adapter for one notification channel. Implementations must be **stateless**: one
// instance is reused concurrently by many channel configurations, so credentials always come in through cfg.
type Channel interface {
	// Kind returns the channel type identifier, which must match the registry key.
	Kind() string
	// Validate is called when configuration is saved, checking required fields and formats. The error
	// is shown directly to whoever is configuring it, so the wording must say **which field is missing**
	// rather than a vague "invalid configuration".
	Validate(cfg map[string]any) error
	// Send delivers one message and returns the **number of items actually delivered** plus an error.
	//
	// Why the count matters: every platform caps message length, so a digest message gets truncated
	// when the whole batch does not fit. If the caller unconditionally marked the whole batch as
	// delivered, the truncated items would simply vanish -- absent from the message, shown as
	// successful in the delivery history, with nowhere left to notice that a finding was never sent.
	// With kept returned, the caller marks only the first kept items and leaves the rest for the next batch.
	//
	// A returned error means delivery failed; a *PermanentError means it must not be retried.
	// On failure kept is meaningless and the caller should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the per-minute delivery cap officially recommended for the channel,
	// used as the default rate limit for a new channel instance. 0 means no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the configuration keys that hold credentials. Their values are masked when the
	// API echoes the configuration back, and a masked value on update keeps the stored original. Only
	// the implementation knows which fields count as credentials (for WeCom the entire webhook URL is
	// the credential, whereas for DingTalk it is only the secret within it), so this knowledge has to
	// come from the channel and cannot be guessed by a layer above.
	SecretKeys() []string
	// DestinationKeys returns the configuration keys that decide **where the message is sent**.
	//
	// Like SecretKeys this is security-relevant: the destination and the credential are two independent
	// sets of fields, and allowing "change only the address, keep the credential" would let anyone who
	// can edit a channel configuration send the stored real credential to a server they control, which
	// would make masking the configuration pointless.
	// See PrepareConfigUpdate for details.
	DestinationKeys() []string
}

// registry is the channel registry. Deliberately an explicit literal rather than init() self-registration:
// that way "which channels exist" is visible in one place, and adding a channel surfaces omissions at
// compile time instead of through a runtime side effect.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get returns the channel implementation for a type.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns every supported channel type in lexicographic order (so the UI dropdown is stable).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that must not be retried: bad credentials, a rejecting
// destination, an illegal request body and so on. Retrying only helps for transient faults (network
// blips, rate limits, a 5xx from the peer); backing off and retrying a permanent failure never
// succeeds and buries the real error in retry logs.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. It returns nil for a nil err, so it can be written as
// `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether the error chain carries the permanent-failure marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- configuration read helpers ----
//
// Channel configuration comes from a JSONB column in the database, so after encoding/json it is a
// map[string]any where numbers are always float64 and arrays are []any. The helpers below normalize
// that layer and tolerate the type drift a user causes by leaving a UI field blank (or typing a port as a string).

// cfgString reads a string setting, trimming surrounding whitespace -- copy-pasting into a web form easily picks it up.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt reads an integer setting, accepting both float64 (the JSON default) and string sources.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool reads a boolean setting, also accepting the strings "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings reads a string-array setting, trimming whitespace and dropping empty strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// A single string is also accepted, which makes a form submission with one value easier.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap reads a string-map setting (e.g. custom HTTP headers), trimming keys and values and dropping empty keys.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
