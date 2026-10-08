package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is the most important invariant in the package. WeCom limits by **bytes**, a CJK character
	// is 3 bytes, and any implementation that slices by byte cuts a character in half, producing
	// invalid UTF-8 that the platform rejects.
	// Inputs of mutually prime lengths mixing ASCII and multi-byte text hit every possible cut point.
	inputs := []string{
		"ünïcödé tëst cöntént",
		"mixed ünïcödé content",
		"aüb€cédöe",
		"🔴🟠🟡🔵", // 4-byte emoji, where a bad cut is even more obvious
		strings.Repeat("vüln€", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("input %q max=%d: produced invalid UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("input %q max=%d: the result is %d bytes, over the cap", in, max, len(got))
			}
			// Content must not be altered when it was not truncated.
			if len(in) <= max && got != in {
				t.Fatalf("input %q max=%d: content changed although it was under the cap -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should mean unlimited")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should mean unlimited")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// When max is smaller than the ellipsis itself, appending the ellipsis must not push it over the cap.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("with max=1 the result %q is %d bytes, over the cap", got, len(got))
	}
	// The normal case should carry an ellipsis.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("expected an ellipsis, got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// The difference in unit from TruncateBytes must be preserved: Telegram limits by character, and
	// using the byte unit would cut a non-ASCII message down to a third.
	s := "áéíóúàèìòù"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("expected 5 characters, got %d (%q)", n, got)
	}
	// The same string measured in bytes should come out noticeably shorter.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("the byte unit must not produce the same character count as the character unit")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("first line\n\nsecond line\twith a tab   and spaces", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("all whitespace should be folded, got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("runs of spaces should not be kept, got %q", got)
	}
	// It must still be readable and valid after truncation.
	got = OneLine("áéíóúàèìòù", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("expected 4 characters, got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Truncating HTML directly cuts out fragments like `<a href="htt`, and the platform rejects the whole message.
	s := `<b>Title</b>body body body<a href="https://example.com/very/long/path">View details</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: the result is %d characters, over the cap", max, n)
		}
		// The tail must not carry an unclosed `<` (i.e. a `<` in the final segment with no `>`).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: the trailing tag was cut -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("no assets should return an empty string, got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("everything should be listed when under the cap, got %q", got)
	}
	// Over the cap it must state the total, or the reader does not know how many assets are missing.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "and 5 in total") {
		t.Fatalf("the total 5 should be stated, got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("an empty severity has ordinal 0 and should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("an empty threshold should let everything through")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("wrong status mapping, got %q", got)
	}
	// An unknown status is echoed back verbatim rather than invented.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("an unknown status should be echoed back verbatim, got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers an omission the audit pointed out: truncation must avoid not
// only half a tag but also a truncated HTML entity.
//
// Once `&amp;` is cut down to `&amp`, a strict parser may reject the **entire** message -- and an
// oversized digest message is common enough that the cost is too high.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The tail must not leave an entity fragment with an & and no matching ;.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: an entity fragment was left at the tail %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: a malformed entity appeared", max)
		}
	}
}
