package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard cap on a WeCom group bot's markdown content (bytes, not characters).
// It is the tightest limit of all six channels, and the main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements the WeCom group bot.
//
// Platform characteristics:
//   - It authenticates solely with the key in the URL and does not support signing, so the webhook address itself is the entire credential.
//   - markdown content is capped at 4096 **bytes** and anything over is rejected outright (not truncated).
//     At 3 bytes per CJK character that leaves only around a thousand characters of body, so client-side truncation is mandatory.
//   - Rate limited to 20 messages per minute, again absorbed by client-side rate limiting.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has exactly one credential (the key in the URL) and does not support signing -- the whole
// address is the entire credential, and no other field needs masking.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has a single webhook field that is both destination and credential, so there is no such thing as "a credential left over after the address changed".
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("the Webhook address is missing")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook address: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A digest batch can be long (50 items x one line each + a prefix), so 4096 bytes is easy to exceed.
	// Truncation happens here rather than relying on the platform to reject it: rejection loses the whole batch, while truncation at least delivers the first few.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse the WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 means the API call quota was exceeded -- the platform's rate-limit window rolls, so
		// retrying after a backoff does work and it is explicitly classed as retryable. Reaching this
		// point means the client-side rate_per_min is set too aggressively; retrying is only a backstop
		// and the real fix is lowering that channel's rate limit.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate limit %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 means the webhook key is invalid -- a permanent failure that retrying will not heal.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
