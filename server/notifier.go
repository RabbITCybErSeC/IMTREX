package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Global setting keys (stored in the settings key-value table, so no new table is needed).
const (
	// settingNotifyEnabled is the master notification switch. On by default: it exists to stop the
	// bleeding during maintenance, not as the feature's enablement condition -- the real condition is "is a channel configured".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the externally reachable address used to build finding detail back-links
	// (such as https://artex.example.com). Left empty, messages carry no back-link button.
	// The project has no reusable external-address setting, so this one is added here.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest mode's period (minutes).
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine's polling interval. 3 seconds is the ceiling on this engine's
	// latency and the main source of delay between "the finding is stored" and "the message arrives in the IM client".
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration when a delivery is claimed. It must be significantly larger than
	// the worst-case duration of one delivery (the notify package's HTTP client times out at 15 seconds), or the same row would be delivered by two
	// dispatchers at once.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick caps how many events are dispatched per round, so enabling a channel for the
	// first time does not expand the entire historical backlog into delivery tasks at once.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default digest period.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-round delivery cap for a channel with no rate limit.
	// It exists to stop "a channel configured without a rate limit + a scan turning up a thousand findings" from
	// turning one loop iteration into a long block.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is how many deliveries one channel makes per round at most.
	//
	// This cap is derived from the **lease duration**: claiming stamps the row with a lease (notifyLease = 3 minutes),
	// and if a round delivers serially often enough that the worst case exceeds the lease, the lease
	// expires before the last few are sent.
	// Within one process that does not matter (Run is a single serial goroutine and a tick never
	// re-enters), but when **two processes share one database**
	// the other one reclaims the expired row and sends it again, double-increments
	// attempts, and judges it failed while the original process is still delivering.
	//
	// The value: a 3-minute lease / a 30-second per-send timeout = 6 would use the lease **exactly** with zero margin,
	// so it cannot be used; 5 keeps the worst case at 150 seconds and leaves 30 seconds of margin. That relationship is pinned down by
	// TestNotifyTickBudgetFitsWithinLease -- changing notifyLease,
	// notifySendTimeout or this value breaks that assertion.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout of one delivery. It also determines the constant above, and
	// their product must not exceed notifyLease; see TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the retry backoff sequence, indexed by the number of attempts already made.
// Its 3 chances (including the first) correspond to db.MaxNotifyAttempts, and the two must be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the finding notification delivery engine.
//
// It runs as an independent goroutine alongside the Scheduler (see server.New). It deliberately does
// not reuse the Scheduler's tick: notifications need near-realtime latency (3 seconds) which differs from the triggers' cadence,
// and their failures are unrelated -- a stuck notification must not affect agent triggers.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu guards buckets. There are few channels and little contention, so one mutex is enough
	// and a finer-grained structure is not worth it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is one channel's token bucket.
//
// A token bucket is used rather than a "count per minute and reset" sliding window because the latter has nasty boundary effects:
// sending 20 at the end of a window and another 20 an instant later is 40 in one second as far as the platform is concerned
// and gets rate-limited; a token bucket refills at a constant rate and avoids that burst naturally.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx ends. Started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step runs one round: dispatch new events first, then deliver the tasks that are due.
//
// A failure at any step is only logged and never breaks the loop -- a fault in the notification system must never escalate into a process-level problem.
// Every tick is independent and the next round retries naturally.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to dispatch events: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] failed to read the channels: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket's unit is **messages** (equivalently, HTTP requests), not findings.
		// In realtime mode they are the same (one finding, one message); in digest mode a whole batch of findings becomes
		// one message and therefore consumes one token.
		//
		// Both modes ask the token bucket first and then claim within that allowance -- the order cannot be reversed, or a delivery blocked by the rate limiter
		// has already consumed a retry.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan returns a digest channel's token consumption and batch size cap for this round.
//
// The two return values are in **different units**, which is exactly why this is its own function:
//
//   - tokens is a number of messages. One batch of findings becomes one message and one HTTP request, so it is always 1.
//     rate_per_min therefore still applies to digests (at most that many digest messages per minute).
//   - claimLimit is how many findings fit in this batch. It is bounded only by memory and is unrelated to the request budget.
//
// To make rate_per_min apply to digests, the per-round request budget
// (notifyMaxSendsPerChannelPerTick, derived from the lease) was once passed straight down as the batch size.
// The consequence was that a channel with rate_per_min=20 only refilled 1 token in a 3-second tick, so every digest
// message held 1 finding -- the digest degenerated into "a realtime push with digest wording", readers received a stream of
// "1 new finding in the last 30 minutes", and db.MaxDigestBatchSize was unreachable.
//
// That symptom is hard to catch in an end-to-end test (the existing cases pass a generously large limit to
// stepDigest by hand and bypass the allowance calculation in step), so the decision is kept here and pinned down directly by
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers one channel's realtime tasks, one message per finding.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim realtime deliveries channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// A rendering failure is a local data problem and retrying will not help.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest aggregates a channel's pending deliveries into one message when the batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] failed to decide whether the digest batch is due channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim the digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries whose snapshot was broken and that never made it into the message must be failed
	// explicitly. Otherwise they stay outside
	// included -- in neither the message nor the failure list -- and on a successful send the later bulk
	// marking misses them, leaving them stuck in sending until the lease expires and they are reclaimed over and over.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "the event snapshot could not be parsed, so this finding cannot be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark bad-snapshot deliveries as failed channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries whose snapshot could not be parsed channel=%d", len(skipped), ch.ID)
	}
	// Only those that made it into the message are handed to send: included[i] corresponds strictly to msg.Items[i],
	// and send relies on that correspondence to apply "the channel reported it fit the first K" to the right delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and moves the state on according to the result.
//
// One batch of deliveries (possibly dozens in digest mode) shares one send result: either delivered, or the whole batch retries.
// There is no per-item retry -- a digest is one message, and resending part of it would corrupt the batch semantics.
//
// The one exception is **segmentation caused by the channel's length cap**: the channel reports that only the first K fit,
// so from the K+1st onwards they must wait for the next batch rather than being marked successful along with the rest. Otherwise the findings that were cut
// are in neither the message nor the failure list and vanish entirely.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// One delivery has a timeout so a stuck channel cannot hold up the remaining channels this round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The count a channel reports cannot exceed the number of deliveries; if it does, the rendering
			// layer miscounted, and
			// treating it as "all delivered" and logging the problem is better than corrupting the records.
			log.Printf("[notify] the channel reported %d delivered, more than the %d deliveries channel=%s; treating it as all delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark as delivered channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message hit the channel's length cap: the rest go straight back into the queue for the next tick to continue.
			// DeferDeliveries is used rather than RescheduleDeliveries -- this is not a failure
			// and must not consume the retry budget (claiming already optimistically incremented it, and that is decremented back there).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("this message hit the channel's length cap, so only the first %d were delivered and the rest wait for the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to queue the remaining segment channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel neither errored nor said how many were delivered. Treat it as a failure (with a backoff),
		// so the delivery is not reclaimed over and over without ever being marked.
		err = fmt.Errorf("the channel did not report how many were delivered (delivered=%d)", delivered)
	}

	// Failure handling is decided **per item** rather than from the batch's maximum attempt count.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts`, condemning the whole batch, but the attempt
	// counts within a batch differ: an old delivery that had already retried twice (attempts=2) would drag the brand new
	// deliveries in the same batch (attempts=1) into failed with it -- a new finding would be lost forever without using a single retry,
	// the exact opposite of the intent "do not let an old row drag a new one down".
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] error marking the failed state channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failing after %d retries: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] error marking the failed state channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Regroup by delay before rescheduling: there are only 3 backoff steps so the number of groups is naturally tiny, and sending one
	// UPDATE per item would mean 500 round trips for a 500-item batch.
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] failed to reschedule deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent=%d retries exhausted=%d awaiting retry=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns the entries of all that are not in keep (compared by pointer identity).
// It is used to find the deliveries that "never made it into the message" -- they must be handled explicitly and not left in limbo.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt fetches the channel implementation and parses its configuration.
// ok=false means the type is not registered and the delivery should be failed outright rather than retried forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// On a configuration parse failure, pass an empty map: the channel's own Validate reports "which field is missing",
		// and that error guides the user to a fix better than a JSON parse error would.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single-finding message.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch renders a digest message. Snapshots are parsed one by one -- a single bad one is skipped
// without taking the whole digest down with it.
//
// The returned included corresponds **strictly one to one** with msg.Items (the i-th delivery <-> the i-th entry).
// That correspondence is a hard requirement: the caller decides that the first K deliveries are marked delivered
// from "the channel reported the first K fit". If a bad snapshot were skipped here without also removing its delivery from included,
// the indices would shift -- a bad entry that should fail would be marked delivered while a good one would be wrongly judged undelivered.
// The bad ones are marked failed explicitly by the caller; see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// A bad snapshot enters neither the message nor included -- handling it is the caller's job
			// (marking it failed explicitly, rather than slipping it in among the "delivered").
			log.Printf("[notify] skipped an unparseable snapshot in the digest batch delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in the digest batch were unparseable", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into an item to push, resolving the asset names and the detail back-link along the way.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// A failed asset name lookup must not stop the push: not reading a name is far less serious than not receiving the notification,
		// and the message simply has one fewer asset line.
		log.Printf("[notify] failed to resolve the asset names finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// The detail page route is in web/src/app/(main)/function/findings/detail/page.tsx,
		// which reads the finding id from the id query parameter.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takes **at most want** tokens from the channel's token bucket and returns how many it actually got.
//
// One token = one message (one HTTP request). In realtime mode the caller passes however many it wants;
// in digest mode a whole batch of findings is one message, so it passes 1.
//
// The bucket's capacity is that channel's per-minute cap and it refills at a constant rate. ratePerMin<=0 means no rate limit and
// returns a finite but generous value, so one loop iteration cannot be held up by an unbounded backlog.
//
// The want cap is essential: without it the only option is draining the whole bucket, while the caller has its own per-round cap,
// so the extra tokens are unusable and vanish before the next refill -- the accumulated burst capacity would be permanently unreachable,
// and even "there is nothing pending this round" would still deduct from it.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// Refill by the real elapsed time, at a rate of ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating: the token count is a floating-point accumulation, and refilling in two steps
	// may turn 0.5 + 0.5 into 0.9999999999, which a bare int() truncates to 0 --
	// a bucket that is mathematically full would yield no token. 1e-9 is far smaller than one token, so it never hides a genuine shortfall.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external address used for back-links, with the trailing slash removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest period, falling back to the default when it is invalid or unset.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot parses the snapshot of a delivery's event.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("the event snapshot of delivery %d is empty", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse the event snapshot of delivery %d: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event type comes from the event row; the copy in the snapshot may have been written by an older version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
