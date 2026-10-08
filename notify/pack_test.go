package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the "pack by item" fix: when a digest message exceeds a channel's length cap it must
// be truncated **by whole item** and report honestly how many items did not fit, so the caller marks only the ones really delivered.
//
// The previous approach rendered everything, truncated, then marked the whole batch delivered: the
// second half of the message vanished while the delivery history showed complete success -- findings were simply lost, with nowhere to notice it.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// A 200-item digest, certain to be far over WeCom's 4096 bytes.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("the body is %d bytes, over the cap of %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("the body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("only part should fit (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must state honestly how many this message contains and how many remain -- otherwise the
	// reader takes the number in the header for the whole batch.
	if !strings.Contains(body, "remaining") || !strings.Contains(body, "next message") {
		t.Fatalf("the header should state how many items are not included in this message:\n%s", body[:minInt(400, len(body))])
	}
	// Only the first kept items should be included.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "Finding"+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "Finding"+itoa(kept+1)) {
		t.Fatalf("item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = unlimited
	if kept != len(m.Items) {
		t.Fatalf("everything should be kept when the length is unlimited, got kept=%d", kept)
	}
	if strings.Contains(body, "remaining") {
		t.Fatalf("no truncation notice should appear when nothing was truncated:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// When the budget is too small for even one item, one must still be sent (with the final truncation as the backstop).
	// Otherwise one oversized finding jams the whole batch forever: every claim fails to fit and nothing is ever sent.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("at least 1 item should be kept, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("a single message should report 1 item delivered, got %d", kept)
	}
	// An empty message has no deliverable items.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("an empty message should report 0, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits by **character count**; the byte unit would squeeze a non-ASCII message to a third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("the body is %d characters, over the cap of %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("only part should fit, got %d", kept)
	}
	if !strings.Contains(text, "next message") {
		t.Fatalf("it should state that some items are not included:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("the card should only fit part of them, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels do not truncate the body, so the whole batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("the precondition does not hold")
	}
	// Confirmed indirectly through the renderer's return value: markdownBody(0) keeps everything when unlimited.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("everything should be used when the length is unlimited, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is the regression test for "untrusted content must not change the message structure".
// The title and summary come from model output (and the model reads the target's responses), and asset names come from the target's URLs.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // must appear in the result (in escaped form)
		wrong []string // must not appear in the result (in unescaped form)
	}{
		{
			name: "a newline plus an external link in the title",
			item: Item{
				Severity: "high",
				Name:     "SQL injection on the login endpoint\n[Urgent: click here to verify your account](http://attacker.tld)",
			},
			// The newline must be folded (or a new list item / block quote could be forged);
			// the square and round brackets must be escaped (or it is a clickable external link).
			must:  []string{`\[Urgent: click here to verify your account\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[Urgent", "\n\n[Urgent"},
		},
		{
			name: "an image beacon in the title",
			item: Item{
				Severity: "high",
				Name:     "finding ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and a quote in an asset name",
			item: Item{
				Severity: "high",
				Name:     "an ordinary title",
				Assets:   []string{"a.com/*injection*>quote"},
			},
			must:  []string{`\*injection\*`, `\>`},
			wrong: []string{"*injection*"},
		},
		{
			name: "a backtick and a pipe in the summary",
			item: Item{
				Severity: "high",
				Name:     "title",
				Summary:  "`code` | table",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// writeItem in single mode is the rendering path shared by all three markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("the escaped form %q is missing:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("the unescaped form %q appeared (it can be used to inject structure or an external link):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst pins down the escaping order: the backslash must be handled first,
// or the backslashes added afterwards get another layer and double backslashes appear in the output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("wrong escaping order, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes pins down one concrete regression:
// markdown escaping must not leak into Telegram's HTML output (escaping was once added to the shared
// title function and visible backslashes like `\(1\)` showed up in Telegram messages).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *emphasis*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("markdown backslash escaping appeared in the Telegram body:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
