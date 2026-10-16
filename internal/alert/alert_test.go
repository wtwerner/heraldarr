package alert

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

var t0 = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

const key = "prowlarr:health:IndexerStatusCheck"

func cfg() Config {
	return Config{
		Hold: 30 * time.Minute, Recheck: 15 * time.Minute,
		RetryInterval: 2 * time.Minute, RetryMax: 3, Updates: true,
	}
}

type rig struct {
	t     *testing.T
	clock *testkit.Clock
	store *testkit.MemStore
	m     *Manager
	drops []string
}

func newRig(t *testing.T, c Config) *rig {
	r := &rig{t: t, clock: testkit.NewClock(t0), store: testkit.NewMemStore()}
	r.m = New(c, r.clock, r.store)
	r.m.Dropped = func(a *domain.Alert, reason string) { r.drops = append(r.drops, a.Key+" "+reason) }
	return r
}

func warning(msg string) domain.AlertEvent {
	return domain.AlertEvent{
		Source: "prowlarr", Kind: domain.AlertHealth, App: "Prowlarr", Check: "IndexerStatusCheck",
		Level: domain.LevelWarning, Message: msg, WikiURL: "https://wiki.example.org/indexers",
	}
}

func restored() domain.AlertEvent {
	return domain.AlertEvent{
		Source: "prowlarr", Kind: domain.AlertRestored, App: "Prowlarr", Check: "IndexerStatusCheck",
		Level: domain.LevelWarning, Message: "Indexers unavailable due to failures: Example",
	}
}

func (r *rig) add(e domain.AlertEvent) Result {
	r.t.Helper()
	res, err := r.m.Add(context.Background(), e)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *rig) due() []Work {
	r.t.Helper()
	w, err := r.m.Due(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

func (r *rig) done(k string, o Outcome, msg *domain.Message) {
	r.t.Helper()
	if err := r.m.Done(context.Background(), k, o, msg); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) stored(k string) *domain.Alert {
	r.t.Helper()
	all, err := r.store.LoadAlerts(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return all[k]
}

func one(t *testing.T, w []Work, action Action) *domain.Alert {
	t.Helper()
	if len(w) != 1 || w[0].Action != action {
		t.Fatalf("due = %+v, want one %v", w, action)
	}
	return w[0].Alert
}

var msg = &domain.Message{Destination: "server", ID: "m1", Layout: "v2"}

func TestWarningRestoredWithinHoldIsNeverPosted(t *testing.T) {
	r := newRig(t, cfg())
	if got := r.add(warning("Indexers unavailable due to failures: Example")); got != Held {
		t.Fatalf("Add = %v, want held", got)
	}
	r.clock.Advance(10 * time.Minute)
	if w := r.due(); len(w) != 0 {
		t.Fatalf("due inside the hold: %+v", w)
	}
	if got := r.add(restored()); got != Cleared {
		t.Fatalf("restore = %v, want cleared", got)
	}
	// Kept for one more hold in case it fails again, then dropped without a post.
	r.clock.Advance(29 * time.Minute)
	if w := r.due(); len(w) != 0 || len(r.drops) != 0 {
		t.Fatalf("due = %+v drops = %v", w, r.drops)
	}
	r.clock.Advance(2 * time.Minute)
	if w := r.due(); len(w) != 0 {
		t.Fatalf("a cleared alert came due: %+v", w)
	}
	if len(r.drops) != 1 || r.drops[0] != key+" cleared" || r.stored(key) != nil {
		t.Errorf("drops = %v, stored = %+v; want dropped as cleared and deleted", r.drops, r.stored(key))
	}
}

func TestWarningOpenPastHoldPostsOnce(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("Indexers unavailable due to failures: Example"))
	r.clock.Advance(29*time.Minute + 59*time.Second)
	if w := r.due(); len(w) != 0 {
		t.Fatalf("due before the hold ends: %+v", w)
	}
	r.clock.Advance(time.Second)
	a := one(t, r.due(), Post)
	if a.App != "Prowlarr" || a.Check != "IndexerStatusCheck" || a.WikiURL == "" || !a.First.Equal(t0) {
		t.Errorf("handed out %+v", a)
	}
	if w := r.due(); len(w) != 0 {
		t.Fatalf("handed out twice while in flight: %+v", w)
	}
	r.done(key, Sent, msg)
	if s := r.stored(key); s == nil || s.Posted == nil || s.Posted.ID != "m1" || !s.PostedAt.Equal(r.clock.Now()) {
		t.Fatalf("stored after post: %+v", s)
	}
	r.clock.Advance(time.Minute)
	if w := r.due(); len(w) != 0 {
		t.Fatalf("posted alert due again: %+v", w)
	}
}

func TestErrorsUseTheirOwnHold(t *testing.T) {
	r := newRig(t, cfg()) // HoldErrors 0
	e := warning("Unable to communicate with the download client")
	e.Check, e.Level = "DownloadClientCheck", domain.LevelError
	r.add(e)
	one(t, r.due(), Post)
}

func TestEscalationToErrorShortensTheHold(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("one indexer down"))
	r.clock.Advance(time.Minute)
	e := warning("all indexers down")
	e.Level = domain.LevelError
	if got := r.add(e); got != Repeat {
		t.Fatalf("Add = %v, want repeat", got)
	}
	if a := one(t, r.due(), Post); a.Message != "all indexers down" || a.Level != domain.LevelError {
		t.Errorf("handed out %+v", a)
	}
}

func TestRepeatHealthUpdatesWithoutPostingAgain(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down: A"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	r.done(key, Sent, msg)
	// The *arr re-sends open issues after it restarts.
	if got := r.add(warning("down: A, B")); got != Repeat {
		t.Fatalf("Add = %v, want repeat", got)
	}
	if w := r.due(); len(w) != 0 {
		t.Fatalf("repeat came due: %+v", w)
	}
	if s := r.stored(key); s.Message != "down: A, B" || s.Posted == nil || !s.First.Equal(t0) {
		t.Errorf("stored %+v", s)
	}
}

func TestFlappingCheckKeepsItsFirstFailure(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(5 * time.Minute)
	r.add(restored())
	r.clock.Advance(20 * time.Minute) // fails again within a hold of the restore
	if got := r.add(warning("down")); got != Repeat {
		t.Fatalf("Add = %v, want repeat (reopened)", got)
	}
	r.clock.Advance(5 * time.Minute) // 30 min after the first failure
	if a := one(t, r.due(), Post); !a.First.Equal(t0) || !a.Resolved.IsZero() {
		t.Errorf("handed out %+v", a)
	}
}

func TestRestoredAfterPostResolves(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	r.done(key, Sent, msg)
	r.clock.Advance(time.Hour)
	if got := r.add(restored()); got != Restored {
		t.Fatalf("Add = %v, want restored", got)
	}
	a := one(t, r.due(), Resolve)
	if a.Posted == nil || a.Posted.ID != "m1" || !a.Resolved.Equal(r.clock.Now()) {
		t.Errorf("handed out %+v", a)
	}
	r.done(key, Closed, nil)
	if s := r.stored(key); s != nil {
		t.Errorf("still stored after resolving: %+v", s)
	}
	if got := r.add(restored()); got != Ignored {
		t.Errorf("restore of an unknown issue = %v, want ignored", got)
	}
}

func TestRecheck(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	r.done(key, Sent, msg)
	r.clock.Advance(14 * time.Minute)
	if w := r.due(); len(w) != 0 {
		t.Fatalf("recheck before its interval: %+v", w)
	}
	r.clock.Advance(time.Minute)
	one(t, r.due(), Recheck)
	r.done(key, Open, nil)
	r.clock.Advance(14 * time.Minute)
	if w := r.due(); len(w) != 0 {
		t.Fatalf("still-open recheck didn't restart the interval: %+v", w)
	}
	r.clock.Advance(time.Minute)
	one(t, r.due(), Recheck)
	r.done(key, Gone, nil) // the restore event was missed
	a := one(t, r.due(), Resolve)
	if !a.Resolved.Equal(r.clock.Now()) {
		t.Errorf("resolved at %v, want now", a.Resolved)
	}
}

func TestHeldAlertGoneFromTheAppIsDropped(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	r.done(key, Closed, nil)
	if s := r.stored(key); s != nil || len(r.drops) != 1 || r.drops[0] != key+" cleared" {
		t.Errorf("stored = %+v drops = %v; want dropped as cleared", s, r.drops)
	}
}

func TestReportedAgainWhileBeingCheckedIsKept(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	r.add(warning("down again")) // the app's health said passing, but it just failed again
	r.done(key, Closed, nil)
	if a := one(t, r.due(), Post); a.Message != "down again" {
		t.Errorf("handed out %+v", a)
	}
}

func TestFailedDeliveryRetriesThenDrops(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	for try := 1; try < 3; try++ {
		one(t, r.due(), Post)
		r.done(key, Failed, nil)
		if w := r.due(); len(w) != 0 {
			t.Fatalf("try %d: due again before the retry interval: %+v", try, w)
		}
		r.clock.Advance(2 * time.Minute)
	}
	one(t, r.due(), Post)
	r.done(key, Failed, nil) // third failure: RetryMax
	if s := r.stored(key); s != nil || len(r.drops) != 1 || r.drops[0] != key+" failed" {
		t.Errorf("stored = %+v drops = %v; want dropped as failed after RetryMax", s, r.drops)
	}
}

func TestRestoreWhilePostingIsKept(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	r.add(restored()) // arrives while the card is being posted
	r.done(key, Sent, msg)
	if a := one(t, r.due(), Resolve); a.Posted == nil {
		t.Errorf("handed out %+v", a)
	}
}

func TestFailureWhileResolvingStartsOver(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	r.done(key, Sent, msg)
	r.add(restored())
	one(t, r.due(), Resolve)
	r.add(warning("down again")) // fails again while the card is being marked resolved
	r.done(key, Closed, nil)
	s := r.stored(key)
	if s == nil || s.Posted != nil || !s.Resolved.IsZero() || !s.First.Equal(r.clock.Now()) {
		t.Fatalf("stored %+v; want a new held alert", s)
	}
}

func TestUpdates(t *testing.T) {
	up := domain.AlertEvent{Source: "sonarr", Kind: domain.AlertUpdate, App: "Sonarr", Message: "Sonarr updated", From: "4.0.1", To: "4.0.2"}
	t.Run("posted at once", func(t *testing.T) {
		r := newRig(t, cfg())
		if got := r.add(up); got != Update {
			t.Fatalf("Add = %v", got)
		}
		a := one(t, r.due(), Post)
		if a.Key != "sonarr:update:4.0.2" || a.Kind != domain.AlertUpdate || a.From != "4.0.1" {
			t.Errorf("handed out %+v", a)
		}
		r.done(a.Key, Sent, msg)
		if s := r.stored(a.Key); s != nil {
			t.Errorf("update kept after posting: %+v", s)
		}
	})
	t.Run("off", func(t *testing.T) {
		c := cfg()
		c.Updates = false
		r := newRig(t, c)
		if got := r.add(up); got != Ignored {
			t.Fatalf("Add = %v, want ignored", got)
		}
		if w := r.due(); len(w) != 0 {
			t.Errorf("due: %+v", w)
		}
	})
}

func TestSurvivesRestart(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post) // handed out, then the process dies before Done
	m := New(cfg(), r.clock, r.store)
	if a := one(t, mustDue(t, m), Post); !a.First.Equal(t0) {
		t.Errorf("after restart: %+v", a)
	}
}

func mustDue(t *testing.T, m *Manager) []Work {
	t.Helper()
	w, err := m.Due(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestDuplicateRestoreIsIgnored(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.add(restored())
	r.clock.Advance(20 * time.Minute)
	if got := r.add(restored()); got != Ignored {
		t.Fatalf("second restore = %v, want ignored", got)
	}
	if s := r.stored(key); !s.Resolved.Equal(t0) {
		t.Errorf("resolved moved to %v; a duplicate must not extend the hold", s.Resolved)
	}
}

func TestRecheckZeroMeansNever(t *testing.T) {
	c := cfg()
	c.Recheck = 0
	r := newRig(t, c)
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	r.done(key, Sent, msg)
	r.clock.Advance(24 * time.Hour)
	if w := r.due(); len(w) != 0 {
		t.Fatalf("rechecked with recheck 0: %+v", w)
	}
}

func TestFailedResolveGivesUpAfterRetryMax(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	r.done(key, Sent, msg)
	r.add(restored())
	for try := 1; try <= 3; try++ {
		one(t, r.due(), Resolve)
		r.done(key, Failed, nil)
		r.clock.Advance(2 * time.Minute)
	}
	if s := r.stored(key); s != nil {
		t.Errorf("still stored after RetryMax failed resolves: %+v", s)
	}
	if len(r.drops) != 0 {
		t.Errorf("drops = %v; a posted alert isn't dropped unposted", r.drops)
	}
}

func TestTriesResetOnceSent(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	for range 2 {
		one(t, r.due(), Post)
		r.done(key, Failed, nil)
		r.clock.Advance(2 * time.Minute)
	}
	one(t, r.due(), Post)
	r.done(key, Sent, msg)
	r.add(restored())
	one(t, r.due(), Resolve)
	r.done(key, Failed, nil) // one failed resolve must not use up the post's retries
	if s := r.stored(key); s == nil || s.Tries != 1 {
		t.Errorf("stored %+v; want tries 1", s)
	}
}

func TestReportedAgainDuringRecheckStaysOpen(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	r.done(key, Sent, msg)
	r.clock.Advance(15 * time.Minute)
	one(t, r.due(), Recheck)
	r.add(warning("down again")) // the app's health said passing, then it failed again
	r.done(key, Gone, nil)
	if s := r.stored(key); s == nil || !s.Resolved.IsZero() {
		t.Fatalf("stored %+v; want still open", s)
	}
	if w := r.due(); len(w) != 0 {
		t.Errorf("due = %+v", w)
	}
}

func TestRefreshKeepsTheAppsMessage(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down: A"))
	if err := r.m.Refresh(context.Background(), key, "down: A, B"); err != nil {
		t.Fatal(err)
	}
	if s := r.stored(key); s.Message != "down: A" {
		t.Errorf("refreshed an alert that wasn't handed out: %q", s.Message)
	}
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	for _, m := range []string{"down: A, B", ""} { // empty: keep it
		if err := r.m.Refresh(context.Background(), key, m); err != nil {
			t.Fatal(err)
		}
	}
	r.add(warning("down: A, B, C")) // an event still wins
	r.done(key, Sent, msg)
	if s := r.stored(key); s.Message != "down: A, B, C" {
		t.Errorf("message %q", s.Message)
	}
	r.add(restored())
	if a := one(t, r.due(), Resolve); a.Message != "down: A, B, C" {
		t.Errorf("resolve shows %q", a.Message)
	}
}

// failingSaves is a MemStore whose SaveAlert fails while fail is set.
type failingSaves struct {
	*testkit.MemStore
	fail bool
}

func (f *failingSaves) SaveAlert(ctx context.Context, a *domain.Alert) error {
	if f.fail {
		return errors.New("disk I/O error")
	}
	return f.MemStore.SaveAlert(ctx, a)
}

func TestRefreshIgnoresAnEmptyMessage(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down: A"))
	r.clock.Advance(30 * time.Minute)
	one(t, r.due(), Post)
	if err := r.m.Refresh(context.Background(), key, ""); err != nil {
		t.Fatal(err)
	}
	if s := r.stored(key); s.Message != "down: A" {
		t.Errorf("message %q", s.Message)
	}
}

func TestFailedAddDoesntCountAsAnEvent(t *testing.T) {
	clock, st := testkit.NewClock(t0), &failingSaves{MemStore: testkit.NewMemStore()}
	var drops []string
	m := New(cfg(), clock, st)
	m.Dropped = func(a *domain.Alert, reason string) { drops = append(drops, reason) }
	ctx := context.Background()
	if _, err := m.Add(ctx, warning("down")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Minute)
	one(t, mustDue(t, m), Post)
	st.fail = true
	if _, err := m.Add(ctx, warning("down again")); err == nil {
		t.Fatal("Add with a failing store returned no error")
	}
	st.fail = false
	if err := m.Done(ctx, key, Closed, nil); err != nil { // the app says it passes
		t.Fatal(err)
	}
	if len(drops) != 1 {
		t.Errorf("drops = %v; an Add that wasn't saved must not keep the alert", drops)
	}
}

func TestDueHandsOutCopies(t *testing.T) {
	r := newRig(t, cfg())
	r.add(warning("down"))
	r.clock.Advance(30 * time.Minute)
	a := one(t, r.due(), Post)
	a.Message = "changed by the caller"
	r.done(key, Failed, nil)
	r.clock.Advance(2 * time.Minute)
	if b := one(t, r.due(), Post); b.Message != "down" {
		t.Errorf("message %q: Due shared its alert", b.Message)
	}
}

func TestDuplicateUpdateIsIgnored(t *testing.T) {
	r := newRig(t, cfg())
	up := domain.AlertEvent{Source: "sonarr", Kind: domain.AlertUpdate, Message: "m", To: "4.0.2"}
	r.add(up)
	if got := r.add(up); got != Ignored {
		t.Errorf("second update = %v, want ignored", got)
	}
}
