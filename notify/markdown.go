package notify

import (
	"fmt"
	"strings"
)

// This file holds the message rendering shared by the "Markdown family" channels (DingTalk, WeCom).
// Feishu uses card JSON, Telegram uses HTML and email uses HTML, each rendered in its own adapter.

// maxAssetsShown is how many assets a message lists at most. A finding can be anchored to dozens of
// assets, and listing them all swamps the message with no informational value -- nobody reads the fourth domain onwards in an IM client.
const maxAssetsShown = 3

// maxSummaryRunes is how many characters the summary is compressed to. An IM message is a "go look at
// the details" prompt, not the report itself; the full content lives in the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is held back for the message header (digest line + severity breakdown + a
// possible truncation notice) and footer (platform link). Packing by item subtracts it from the budget
// so the header and footer can never be cut -- once they are, the reader cannot even tell which batch
// this is or how many items are still missing.
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
//
// Why it is mandatory: the finding title, summary, class and asset display names all come from
// **untrusted sources** -- the title and summary come from model output (and the model reads the
// target's responses), while an asset's url is the full scanned URL (including a target-controlled query string).
// Without escaping, a finding whose title is
//
//	SQL injection on the login endpoint\n[Urgent: click here to verify your account](http://attacker.tld)
//
// would render as a **clickable external link** in a security engineer's DingTalk/Feishu; and
// `![](http://attacker.tld/beacon)` would be fetched by the client at render time, announcing that
// "this finding has been read" and leaking the reader's IP. Even with harmless content, injected bold
// text or a block quote can push the critical finding below the fold.
//
// The escape set covers the characters for headings/links/emphasis/lists/quotes/strikethrough, i.e.
// everything that changes the structure or produces a clickable element. `\` must be handled first, or
// it would re-escape the backslashes added afterwards.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText collapses untrusted text onto one line and escapes it, for use in a markdown body.
// Collapsing to one line is the other half of escaping: a newline alone can forge a new list item or
// block quote, and escaping characters does not stop it.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the title bar / card title on an IM platform), as **unescaped raw text**.
//
// Escaping is deliberately not done here: this title is shared by four rendering contexts -- a markdown
// body, Telegram's HTML, a Feishu card's plain_text, and the JSON of a generic webhook plus the email
// subject. Each context has different escaping rules (markdown escapes embedded in HTML leave visible
// backslashes, and embedded in JSON they corrupt the data), so escaping must be the responsibility of
// each output side; see writeItem / feishuItemLines / telegramEscape. Markdown escaping was once added
// to the shared function and the result was visible backslashes like `\(1\)` in Telegram messages.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Findings digest - %d in total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Finding notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body and returns the body plus the **number of items actually written**.
//
// The returned kept is the number of items this delivery really sent, and the caller marks only the
// first kept items as delivered -- items kept out by the channel's length cap must wait for the next
// batch rather than being marked successful along with the rest. That is exactly where "silent loss"
// comes from: the message was truncated, but the delivery record says everything was delivered, and
// nothing anywhere shows that the second half was never sent.
//
// maxBytes<=0 means unlimited.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// A single message is sent even when oversized (the final truncation is the backstop): partial
		// information about one finding beats sending nothing at all.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all in the platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the opening of a digest message: time window, count and severity breakdown.
// With those, whoever receives the digest can judge whether it needs immediate attention without opening the platform.
//
// items are the ones **actually packed**, total is how many the batch should contain. When they differ
// it must say **how many more are in the next message** -- otherwise the reader assumes the number in
// the header is everything, while the items that were never sent do not exist anywhere in the UI.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**%d new findings in the last %d minutes**", total, m.WindowMinutes)
	} else {
		fmt.Fprintf(&b, "**%d new findings**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, " (this message shows the first %d; the remaining %d follow in the next message)", len(items), extra)
	}
	// Give the breakdown by severity so the reader sees at a glance whether anything is critical. Only
	// the items **actually contained in this message** are counted, so "Critical 3" matches the items you can count below.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " - "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders a single finding item.
//
// prefix supplies the index in a digest list; single=true renders the full version (with summary and
// back-link), while a digest list renders only a one-line summary -- otherwise a 50-item digest turns into a long document.
//
// Everything coming from outside (title/class/assets/summary) goes through markdownText: collapsed to
// one line and escaped. The back-link is assembled from the administrator-configured public_base_url,
// is not untrusted content, and has to be clickable, so it is emitted as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s - %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: one line, with the assets and summary compressed and appended.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " - " + strings.Join(extras, " - ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s -> %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**Class**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Assets**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**Summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}
