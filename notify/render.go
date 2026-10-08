package notify

import (
	"strings"
	"unicode/utf8"
)

// A single-rune ellipsis: TruncateRunes/TruncateHTML budget it as exactly one character.
const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, guaranteeing valid UTF-8 and never splitting a character.
//
// Why it must cut on a character boundary: the WeCom group bot's markdown has a hard limit of 4096
// **bytes** (not characters), and a CJK character is 3 bytes. Slicing by byte would cut a character
// in half and produce invalid UTF-8 -- the platform either rejects the whole message or renders
// garbled boxes. The approach here is to walk back from the budget position to the nearest rune start
// byte (utf8.RuneStart identifies continuation bytes 0b10xxxxxx).
//
// max<=0 means unlimited. An ellipsis is appended after truncation, unless max is too small to hold it.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis itself: drop the ellipsis and truncate plainly, so the result does not end up exceeding max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine collapses multi-line text onto one line: all whitespace is folded, then it is truncated by character count.
// Used for the title line of an IM message -- summaries often contain newlines, which would wreck a table or title layout.
// max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), appending an ellipsis when it overflows.
// max<=0 means unlimited.
//
// It differs from TruncateBytes in which unit the platform uses: WeCom limits by bytes, Telegram by character count.
// Using the wrong unit raises no error; it just cuts messages far shorter than intended (a CJK character
// is 3 bytes, so cutting 4096 bytes leaves only about 1365 characters), which is why both functions exist and must be chosen per channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count while guaranteeing no half-finished tag.
//
// Truncating HTML by character directly can cut out a broken tag like `<a href="htt`, and the platform
// parser either rejects the whole message or swallows the rest of the body as an attribute value. The
// approach here is to truncate by character first, then check whether the tail has an unclosed `<` and back up before it.
//
// No tag balancing (auto-closing a dangling </b> and the like): Telegram's HTML parser closes unclosed
// tags itself, and implementing balancing would mean handling quotes inside attributes, comments and
// self-closing tags, at a complexity out of all proportion to the benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with `<` (no `>` after the last `<`), back up to before the `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a truncated HTML entity (e.g. `&amp;` cut down to `&amp`), back up as well.
	// An entity fragment can make a strict parser reject the **entire message** -- and a digest message
	// that exceeds the length cap is common enough that it is not worth losing a whole notification over.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount computes how many items fit **whole** within the budget, so a digest message is packed item by item.
//
// Why pack by item rather than render everything and truncate: truncation makes the trailing items
// vanish while their delivery records are still marked as delivered -- invisible in the message and in
// the delivery history, so the findings are simply lost. Packed by item, whatever does not fit stays in
// the database for the next batch, and the kept value the caller receives is the number of items this message really delivered.
//
// Parameters: maxSize<=0 means unlimited; reserve is the amount held back for the message header/footer;
// size does the measuring (platforms differ: WeCom/DingTalk count bytes, Telegram counts characters --
// using the wrong unit raises no error, it just squeezes non-ASCII messages far below the cap);
// render turns item idx into its actual text, whose length depends on the content and cannot be estimated.
//
// It returns at least 1 while items remain. Even a single extremely long item must be sent, with the
// caller's final truncation as the backstop; otherwise one oversized finding would jam the whole batch forever.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are packItemCount's two measuring units, named so the call sites do not carry a
// bare func(s string) int closure, which would make it hard to see at a glance which unit is in use.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders an asset list as one display line, omitting the rest and noting the total above limit items.
// A finding can be anchored to dozens of assets, and listing them all would swamp the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " and " + itoa(len(assets)) + " in total"
}

// itoa is a short alias for strconv.Itoa, used only for assembling display text so strconv need not be imported everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
