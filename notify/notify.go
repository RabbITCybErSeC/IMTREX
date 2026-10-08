// Package notify implements the IM / email delivery adapters for findings.
//
// Layering: this is a **leaf package** that depends only on the standard library. It knows nothing
// about the database or about server. Channel configuration arrives as map[string]any (mirroring the
// notification_channels.config JSONB column) and the content to deliver arrives as a Message. The
// benefit of splitting it this way is that the genuinely error-prone parts -- signature computation,
// UTF-8 truncation, filter matching -- can be unit-tested without PostgreSQL, leaving the host to do
// only the orchestration on the server side.
//
// Concurrency contract: Channel implementations must be **stateless**. One Channel instance is reused
// concurrently by many channel configurations (even several bot instances of the same channel), so
// every credential comes in through the cfg parameter; caching things like a webhook URL in the
// implementation's own fields is not allowed.
package notify

// Channel type identifiers. The values are also the legal set for notification_channels.kind,
// validated by an allowlist on the server side (same approach as findings.status: no DB CHECK, so
// adding a channel later stays easy).
const (
	KindDingTalk = "dingtalk" // DingTalk custom bot
	KindFeishu   = "feishu"   // Feishu (and Lark) custom bot
	KindWeCom    = "wecom"    // WeCom group bot
	KindWebhook  = "webhook"  // Generic webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event types, mirroring notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback value for an empty kind in the config.
const InitKind = KindDingTalk

// severityRank maps a finding's severity onto a comparable ordinal. An unknown severity returns 0,
// so any min_severity setting keeps unknown severities out -- when in doubt do not push, to avoid flooding on a false positive.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the ordinal of a severity; an unknown severity returns 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the severity name with an emoji, used for message titles and card colours.
// An unknown severity is echoed back verbatim rather than invented.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low"
	default:
		return severity
	}
}

// StatusLabel renders a disposition status for status-change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Handled"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk accepted"
	default:
		return status
	}
}

// AtLeast reports whether severity meets the min threshold. An empty min means no threshold, so everything passes.
// Note that an unknown severity has ordinal 0 and is rejected by any non-empty min (see the severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
