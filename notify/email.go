package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout bound the connection and the whole SMTP session respectively.
// net/smtp has no timeout mechanism of its own, and without these two a stuck peer would hang the
// delivery goroutine forever -- and since the dispatcher processes serially in a single goroutine, that stalls the entire notification system.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel implements SMTP email delivery.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Email has no platform rate limit, but it should not be used to spam either; a relaxed default is given.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Only the password is masked. The SMTP host, account and recipients are not secrets, and masking them would only make editing awkward.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port decide which server the password is handed to, and tls decides whether the transport is
// encrypted. A change to any of the three requires restating the password -- which also makes "turning
// TLS off" a step that must explicitly carry the credential rather than a casual edit.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("the SMTP server address is missing")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("invalid SMTP port (must be 1-65535)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("the sender address is missing")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("at least one recipient address is required")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: upgrade when the peer supports it. Credentials must not be sent over a plaintext session (see the auth note below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth refuses to send credentials over an unencrypted connection (unless the target
			// is localhost). That is the **correct** security behaviour and must not be bypassed, but the
			// reason has to be spelled out -- otherwise the user only sees "unencrypted connection" with no idea what to do about it.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("refusing to send credentials: the connection is not encrypted. Enable TLS, switch to port 465 (implicit TLS), or tick \"Enable TLS\" (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP authentication failed: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("sender %s was rejected", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("recipient %s was rejected", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA failed: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("failed to write the mail body: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("failed to submit the mail: %w", err)
	}
	// A failed Quit does not change the fact that the server accepted the mail, so its error is ignored.
	_ = client.Quit()
	// Email has no length truncation (the whole HTML body is sent), so the entire batch counts as delivered.
	return len(m.Items), nil
}

// emailDial establishes the SMTP connection.
//
// implicitTLS=true uses the "TLS from the first byte" style of port 465; false connects in plaintext on
// 25/587 and then does STARTTLS. The two cannot be mixed: sending a plaintext greeting to port 465 gets the connection dropped.
//
// The session deadline is set **at connection time** (rather than patched in afterwards) because
// net/smtp's Client hides the underlying connection in an unexported field that the outside cannot
// reach; once the connection is handed over, only a pre-set deadline can act as a backstop. That also covers a stall during the handshake.
// Hanging blockInternalDial on Control shares the same guard as the HTTP-family channels. Without it,
// SMTP would be the hole in the whole SSRF defence: setting host to 169.254.169.254 or 127.0.0.1 would
// connect straight away, and when smtp.NewClient's handshake fails it wraps the peer's reply line into
// the error, which reaches last_error and is echoed by the delivery history API, giving a semi-blind
// read primitive; the timing difference between "connection refused" and "timeout" could also be used
// to probe ports. The dial stage is where it actually takes effect, and it covers DNS rebinding too.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the SMTP server: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP handshake failed: %w", err)
	}
	return client, nil
}

// smtpStageError classifies a stage failure as "retryable" or "permanent" based on the SMTP reply code.
//
// Why the distinction is essential: SMTP's 4xx and 5xx mean completely different things --
//   - 4xx (450 greylisted, 451 local error, 452 insufficient storage) is a **temporary** rejection and
//     the correct response is to retry later; greylisting in particular is hit on almost every first delivery.
//   - 5xx (550 no such user, 553 illegal address) is a permanent rejection where retrying is pointless.
//
// Treating everything as permanent would make a greylisting mail server drop **every** push into failed
// after the first attempt -- and that is exactly the case automatic retries exist for.
// The reply code is the first three digits of the error text; when no code can be read it is treated as
// retryable (better one extra attempt than condemning a possibly transient fault because it could not be parsed).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode reads the leading three-digit reply code out of an SMTP error text, returning 0 when there is none.
// net/smtp does not export an error code field, so it has to come from the text, whose format is "450 4.7.1 ...".
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage assembles a complete RFC 5322 message.
//
// The body is base64-encoded for two reasons: SMTP limits a line to 1000 bytes and an HTML body (a
// digest mail especially) easily produces longer lines; and base64 never produces a line starting with
// "." , which avoids SMTP dot-stuffing entirely.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// A non-ASCII subject must be RFC 2047 encoded or clients display it as mojibake.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// Email has no hard length cap, so the body is not truncated.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 wraps at 76 characters, per RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
