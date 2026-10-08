package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel implements the DingTalk custom bot.
//
// Platform characteristics (which drive the trade-offs here):
//   - One bot is limited to 20 messages per minute, and anything over is silently dropped (while HTTP
//     may still be 200), so rate limiting has to be done client side; see DefaultRatePerMin.
//   - Security settings offer one of three options: signing / custom keyword / IP allowlist. Signing is
//     the only one that does not depend on the message content, so only signing is supported (a bare webhook with none of the three also works).
//   - Both success and failure return HTTP 200, distinguished by errcode in the body -- not checking
//     errcode would record a failed delivery as a success.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// A DingTalk webhook address contains the access_token and is itself a credential, so the whole thing is masked.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The destination is the DingTalk webhook address itself; changing it requires restating the signing secret for the new address.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("the Webhook address is missing")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook address: %w", err)
	}
	return nil
}

// Send delivers one message. With a back-link and a single item it uses an ActionCard (with a button), otherwise markdown.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk's markdown body has no documented byte cap, but a cap is still enforced so an abnormally large evidence field cannot blow up.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "View details",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides business errors inside a 200 response.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse the DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 is a signature verification failure and 310000 a keyword mismatch -- both are
		// configuration errors that retrying will not heal.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends the timestamp and sign parameters to the webhook per the official signing rules.
//
// The rule: the string to sign is timestamp + "\n" + secret, the HMAC-SHA256 **key is also the secret**,
// and the result is base64-encoded then URL-encoded. The timestamp is in milliseconds. An empty secret
// returns the address unchanged, supporting bots with signing disabled.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// err is not passed through: url.Parse's error text carries the full address (including the access_token).
		return "", fmt.Errorf("failed to parse the webhook address: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL checks that an address is usable and its scheme supported, and tests literal IP targets against the internal ranges.
//
// Two points of care:
//
//  1. **The error message must be redacted**. url.Parse returns a *url.Error whose Error() carries the
//     **full original address**, and these providers embed credentials in the address (DingTalk
//     access_token, WeCom key, Telegram bot token, Feishu hook id). This once did a plain `return err`,
//     so the "malformed address" error carried the credential out into the test endpoint's 400
//     response, the last_error stored on every delivery, the server log and the delivery history API.
//
//  2. **A literal IP is checked against the internal ranges here**, while domain names are left to the
//     dial stage (blockInternalDial is the point where it actually takes effect, and it also covers DNS
//     rebinding). Doing it once here means the user gets a hint when saving the configuration rather
//     than at the first failed delivery.
//
// Restricting the scheme is defensive: file:/// or gopher:// would make http.Client behave unexpectedly
// (already stopped by the scheme check, but there is no reason to widen that surface).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("the address cannot be parsed (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https are supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("the host name is missing")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing to deliver to the loopback/link-local address %s (if you really need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
