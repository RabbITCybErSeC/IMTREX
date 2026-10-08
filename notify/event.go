package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column. It is written by the
// finding-persistence transaction in the db layer and read by the delivery engine and filter matching
// in the server layer. It is defined here because it is the "notification domain" payload: db only
// serializes it and does not interpret the fields.
//
// Why the finding fields are stored redundantly instead of being looked up at render time: a finding
// can later be renamed, re-rated or re-statused, and the pushed content should reflect the conclusion
// **at the time it happened** -- a lookup would produce the dangerously misleading "was later downgraded
// to low". It also means fan-out and rendering need no JOIN across findings/tasks/assets.
type Snapshot struct {
	// Event type: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Non-empty only when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is one finding to be pushed, ready for a channel to render.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets holds the resolved asset display names (domain/IP etc.). Filled in by the server layer --
	// this package never touches the database and cannot look the names up.
	Assets []string
	// DetailURL links back to the finding detail page; empty means public_base_url is unset, so it is omitted when rendering.
	DetailURL string
	// Status-change events only; when both are non-empty they render as "Pending -> Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether this item is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the item's display title: the human-assigned name first, falling back to the
// vulnclass, and a placeholder when both are empty -- never an empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(unnamed finding)"
}

// Message is the complete content of one channel send.
type Message struct {
	// Length 1 for a single push; a whole batch for a digest push.
	// An empty slice is illegal; the caller must guarantee at least one item.
	Items []Item
	// When Batch=true it renders as a digest message (different title, with the time window and count).
	Batch bool
	// WindowMinutes is the digest period in minutes, used only when Batch=true for the "in the last N minutes" wording.
	// Passed in explicitly from the configuration rather than computed with time.Since at render time, so rendering stays deterministic and testable.
	WindowMinutes int
	// HomeURL is the platform dashboard address (the global public_base_url); empty means no dashboard link.
	HomeURL string
}
