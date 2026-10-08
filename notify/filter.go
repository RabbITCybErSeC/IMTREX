package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB column: a channel instance's filter conditions.
// Every field is optional, and absent means "do not filter" -- which is exactly the fallback semantics for a malformed configuration; see ParseFilter.
type Filter struct {
	// MinSeverity is the minimum severity threshold (low/medium/high/critical); empty means no threshold.
	MinSeverity string `json:"min_severity"`
	// Empty TaskIDs / AssetIDs arrays mean unrestricted; non-empty requires the event to intersect them.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// An empty VulnClassInclude accepts everything; non-empty requires the vulnclass to match one of the keywords.
	// Matching any VulnClassExclude keyword excludes the event (exclude wins over include).
	// Matching is case-insensitive substring matching -- safer than a regex: a user's broken regex cannot silently disable the channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange decides whether this channel receives finding status-change events (only meaningful in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses a channel's filter configuration.
//
// **It never returns an error.** That is a deliberate design choice: a malformed filter degrades to a
// zero-value Filter (= no filtering = everything matches), because for a vulnerability notification
// system **pushing one extra message is far better than silently dropping a critical one**. Turning a
// parse failure into "do not push" would give the user a channel that looks configured but pushes
// nothing -- the worst possible failure mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On a parse failure f keeps its zero value, i.e. no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a legal severity threshold (an empty string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate checks the filter fields whose **values are constrained**, for use when saving a channel.
//
// Why this must be caught on write: Match evaluates an unknown threshold as `rank >= 0`, which is
// always true -- so a single typo in min_severity ("hgih") makes the filter **silently ineffective**
// and turns it into "push everything". That is in line with this package's "rather push too much than
// drop something" trade-off (nothing is lost), but the consequence is that the user believes they
// configured severity-based routing while every finding floods the group, with nothing to hint that
// it is misconfigured. This kind of "silent degradation" is exactly what should be caught at the entry point.
//
// Note Validate is only used on the **write** path. The read path keeps ParseFilter's permissive
// semantics, so bad values already stored in historical data never make a channel unreadable as a whole.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("minimum severity %q is invalid; choose one of low / high / medium / critical, or leave it empty for no limit", f.MinSeverity)
	}
	return nil
}

// Match decides whether an event should be delivered to a channel carrying this filter.
//
// **It never returns an error**, for the same reason as ParseFilter: any internal anomaly is treated as a match.
// Evaluation order: event type -> severity threshold -> task/asset scope -> vulnerability class keywords.
func Match(f Filter, s Snapshot) bool {
	// Status-change events are only received by channels that explicitly opt in. Off by default, because
	// the vast majority of users expect "push" to mean "a new finding", not a running commentary on every status transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclude wins: matching any exclude keyword drops the event, even if it also matches the include list.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan over a small set is fine; both sides are of the order "a few dozen hand-picked entries",
	// so building a map would cost more than it saves.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any of the keywords (case-insensitively).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
