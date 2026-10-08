package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets decides whether messages may be delivered to loopback / link-local addresses.
//
// Denied by default. Those ranges are not where an IM bot or a public mail server lives, and what
// they can reach is sensitive: the admin port of another service on the same host, and the cloud
// metadata endpoint (169.254.169.254, which hands out instance credentials). Delivery addresses are
// set by an administrator, but an admin session borrowed via XSS/CSRF, or a second person sharing the
// same JWT, could edit the configuration to read response content back -- doJSON writes the first 200
// bytes of a 4xx/5xx response body into last_error, and the delivery history API echoes it, which is a semi-blind read primitive.
//
// But a "local SMTP relay" (postfix on 127.0.0.1:25) is a common self-hosted mail setup, and blocking
// it outright would leave people stuck. Hence an explicit escape hatch instead of a hard-coded allowance:
// set ARTEX_NOTIFY_ALLOW_LOCAL=1 to permit it.
//
// It is exported as AllowLocalTargetsEnv so tests can enable it explicitly -- the cases in this package
// and in server make heavy use of httptest fake receivers on 127.0.0.1, which the guard would otherwise block.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP falls in a range that delivery is not allowed to reach by default.
//
// It only rejects loopback, link-local (including the cloud metadata address 169.254.169.254), the
// unspecified address and multicast. It does **not** reject RFC1918 private networks: a self-hosted
// Mattermost / SMTP relay on an internal network is a very common legitimate use, and blocking those
// too would make the feature unusable in real deployments. The trade-off is deliberate -- the guard
// must stop the genuinely sensitive targets without breaking normal deployments.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) has to be unwrapped to IPv4 before the check, or it bypasses it.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the Control hook of the http.Transport dialer, checking the target address
// **when the connection is established**.
//
// Why it sits at the dial stage rather than only validating on save: this is the point where it
// actually takes effect. It covers both ways around a save-time check -- DNS rebinding (resolving to a
// public IP at validation time and an internal one at connection time) and redirects (we already
// reject cross-host hops, but a same-host hop can still point the path elsewhere).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("cannot resolve the target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refusing to deliver to the loopback/link-local address %s (if you really need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport adds only a dial guard on top of the default Transport.
// Clone keeps all the default tuning (connection pool, HTTP/2, timeouts, proxy and so on), so adding one check does not change any other behaviour.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the client shared by all channel deliveries.
//
// It deliberately does **not** reuse the project's global egress proxy (GlobalProxy on the server
// side): that proxy carries traffic to the penetration test target and is often an unstable tunnel,
// and notification availability must not be held hostage by the target network's flakiness. IM pushes
// go direct. The timeout is 15 seconds -- a peer slower than that is effectively down.
//
// Cross-host redirects are refused: delivery addresses for this feature are all of the "one fixed
// endpoint" shape and normally never redirect to another host; and these providers' credentials
// (DingTalk's access_token, WeCom's key, Telegram's bot token) are **in the URL**, so following a
// cross-host hop would hand the credential to the redirect target. Same-host hops (adding a trailing slash) are still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing a cross-host redirect (%s -> %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit caps how much of the response body is read. A misbehaving peer can return something
// enormous, while all we need is the status code and a short error description to show in the delivery history.
const respBodyLimit = 8 << 10

// doJSON sends one request and returns the (length-limited) response body.
//
// A nil payload sends an empty body (for GET, or platforms that require no body).
// Keys and values in headers are attached verbatim, for a generic webhook's custom headers.
//
// Error classification is this function's core job: transport failures and 5xx/408/429 count as
// "retryable", every other 4xx as a "permanent failure" -- retrying a 403 only writes the same error to the log three times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// A serialization failure is a local bug (a configuration field of the wrong type) and retrying will not help.
			return nil, Permanent(fmt.Errorf("failed to build the request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// An illegal URL is most likely a typo by the user, so it is a permanent failure.
		// Again err must not be passed through: url.Parse's error text contains the full address.
		return nil, Permanent(fmt.Errorf("illegal request address: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout -- mostly transient, so leave it to the backoff retry.
		//
		// The error text must be redacted before it travels any further. http.Client.Do returns a
		// *url.Error whose Error() is `Op "full URL": underlying error`, and these providers' credentials
		// are **in the URL** (DingTalk access_token, WeCom key, Feishu hook id, Telegram /bot<token>/).
		// Unredacted, the credential would flow along this string into four places: the last_error column
		// of notification_deliveries (stored in plaintext), the delivery history API response (**bypassing
		// the channel configuration masking**), the server log, and the 502 text the test-send endpoint returns to the frontend.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read the response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limited) and 408 (timeout) are worth retrying; other 4xx are configuration or permission problems where retrying is pointless.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("the peer rate-limited us or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("the peer service is failing (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("the peer rejected the request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet squeezes a response body into one short line for an error message. A response may contain
// newlines and a lot of whitespace, and dropping that straight into last_error wrecks the delivery history page layout.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget reduces a delivery address to "scheme://host/..." for error messages.
//
// This is the only address-redaction rule in the package, and it is deliberately **blunt**: everything
// except the scheme and host is discarded. The reason is that there is no "general and safe" way to
// tell which part of a URL is the credential:
//
//	DingTalk  credential in the query      /robot/send?access_token=xxx
//	WeCom     credential in the query      /cgi-bin/webhook/send?key=xxx
//	Feishu    credential in the **last path segment** /open-apis/bot/v2/hook/<hook_id>
//	Telegram  credential in the **middle of the path** /bot<token>/sendMessage
//
// "Keeping only the useful part" would need a patch per channel, and missing any one of them is a credential leak.
// Keeping the host is already enough for diagnosis (an unresolvable DNS name, an unreachable address or
// a bad certificate are all identifiable), and which bot it is can be recognized from the masked suffix in the channel configuration.
//
// On a parse failure it returns a fixed placeholder -- the raw string is never echoed back.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable address)"
	}
	return u.Scheme + "://" + u.Host + "/..."
}

// redactTransportError strips the address out of a transport-layer error, keeping only the underlying cause.
//
// A *url.Error is {Op, URL, Err} and its Error() prints the URL along with everything else.
// Taking the Err field explicitly bypasses that Error() -- more reliable than replacing strings after
// the fact, because replacement would have to cope correctly with every URL-encoded/escaped variant and easily misses one.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// A non-*url.Error (e.g. the error returned by the redirect policy) can also carry an address, so it goes through redaction too.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces any http(s) address appearing in a piece of text with its redacted form.
//
// A backstop for errors with no structured field to read (redirect policy errors, custom errors from third-party libraries).
// It only recognizes the http/https prefix and splits on whitespace and quotes -- an address contains neither.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
