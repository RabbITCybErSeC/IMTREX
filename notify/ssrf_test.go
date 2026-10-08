package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This file covers two related hardening measures:
//   (1) a delivery address must not turn the server into a pivot into the internal network / cloud metadata (SSRF)
//   (2) address validation errors must not carry the credential out of the address
//
// On the test environment: many cases in this package use httptest fake receivers on 127.0.0.1, which
// the guard blocks by default. So TestMain enables AllowLocalTargetsEnv globally, and every SSRF case
// below clears it explicitly in order to assert the **deny by default** behaviour.

func TestMain(m *testing.M) {
	// Let the ordinary cases reach the local fake receivers; the SSRF cases clear it themselves.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core SSRF assertion: with the default configuration,
// delivery to a loopback address must be refused at the **connection layer**.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // close the escape hatch = default behaviour
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("delivery to a loopback address should not be allowed by default")
	}
	if hit {
		t.Fatal("the request reached the local service -- the guard did not take effect")
	}
	// The error message has to tell the user how to open it up (a local SMTP relay is a legitimate configuration).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the refusal should explain how to allow it explicitly: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the inverse case: it must work once explicitly enabled, or
// legitimate deployments such as a local postfix / internal relay are blocked outright.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("delivery should work once explicitly allowed: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // the cloud metadata endpoint -- the main reason this function exists
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // the IPv4-mapped form must be unwrapped before the check, or it is a bypass
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be rejected", s)
		}
	}
	// RFC1918 private networks are **deliberately allowed**: an internal self-hosted Mattermost / SMTP relay is a common legitimate use.
	// This assertion pins that trade-off down -- if someone later adds a private-network check in
	// passing, this fails and forces a conscious decision (rather than silently breaking a class of deployments).
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (a private network is a common legitimate delivery target)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers the hint given at configuration time: a
// literal IP should be rejected on save rather than at the first failed delivery.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected at configuration time", raw)
		}
	}
	// Public and private addresses pass as usual (private is left to the dial stage, which does not block it).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials is the branch the audit pointed out was missed last time.
//
// When url.Parse **fails** it returns a *url.Error whose Error() contains the full original address.
// Last time only the error returned by http.Client.Do was redacted and this was missed; and the
// "permanent failure path" cases added back then (file://, gopher://, ftp://) all parse successfully
// and take the scheme branch, so them being green proved nothing about this path -- a false guarantee.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // invalid percent escape
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // non-numeric port
		"http://[::1?access_token=" + leakProbeToken,                  // unbalanced brackets
	}
	for _, raw := range cases {
		// First confirm this input really does make url.Parse fail. Without that step a case could
		// silently take a different branch (which is exactly where the earlier false guarantee came from).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q was supposed to fail parsing, otherwise this case does not cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm the channel-layer wrapper does not carry the address out either.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("an illegal address should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the dial guard on the SMTP channel.
//
// The email channel once used a bare net.Dialer and was the only hole in the whole SSRF defence:
// setting host to 169.254.169.254 or 127.0.0.1 connected straight away, and when smtp.NewClient's
// handshake failed it wrapped the peer's reply line into the error, which last_error then echoed
// through the delivery history API -- precisely the semi-blind read primitive the other channels
// already closed; the timing difference between "connection refused" and "timeout" could also be used to probe ports.
//
// This package's TestMain enables AllowLocalTargetsEnv globally (many cases use fake receivers on
// 127.0.0.1), so this case has to clear it itself -- otherwise it would pass whether the guard exists
// or not, which is exactly why no test caught the hole originally.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // close the escape hatch = default behaviour
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("delivering mail to a loopback address should not be allowed by default")
	}
	// The connection must never be established: the guard blocks it in the Control hook, so EHLO is never sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("the SMTP session was established -- the guard did not take effect")
	}
	// The error message has to tell the user how to open it up (a local postfix relay is a legitimate configuration).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the refusal should explain how to allow it explicitly: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired inverse case: once explicitly enabled it
// must deliver normally. An internal self-hosted SMTP / local relay is a very common deployment, and the guard must not block it outright.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("local SMTP should deliver once explicitly allowed: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("no EHLO seen -- the session was not really established")
	}
}
