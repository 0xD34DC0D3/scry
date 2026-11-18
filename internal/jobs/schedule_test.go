package jobs

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/0xD34DC0D3/scry/internal/db"
)

// feedRow is a feed's schedule state; disabled nil means active.
type feedRow struct {
	next     int64
	disabled any
}

// addFeeds inserts feeds in the given states and returns their ids in order.
func addFeeds(t *testing.T, d *db.DB, rows ...feedRow) []int64 {
	t.Helper()
	ids := make([]int64, len(rows))
	for i, r := range rows {
		f, err := db.New(d.Writer).CreateFeed(context.Background(), fmt.Sprintf("https://f%d.example/feed", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Writer.Exec(`UPDATE feeds SET next_fetch_at = ?, disabled_at = ? WHERE id = ?`, r.next, r.disabled, f.ID); err != nil {
			t.Fatal(err)
		}
		ids[i] = f.ID
	}
	return ids
}

func TestClaimDue(t *testing.T) {
	n := now.Unix()
	many := make([]feedRow, claimBatch+5)
	for i := range many {
		// Newest first, so "oldest first" is not just id order.
		many[i] = feedRow{n - int64(i), nil}
	}
	for _, tc := range []struct {
		name  string
		feeds []feedRow
		want  []int // indexes into feeds
	}{
		{"due now and overdue", []feedRow{{n, nil}, {0, nil}, {n - 60, nil}}, []int{0, 1, 2}},
		{"not due", []feedRow{{n + 1, nil}, {n, nil}}, []int{1}},
		{"disabled excluded", []feedRow{{0, 5}, {0, nil}}, []int{1}},
		{"limit honoured, oldest first", many, func() []int {
			var w []int
			for i := 5; i < len(many); i++ {
				w = append(w, i)
			}
			return w
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openDB(t)
			ids := addFeeds(t, d, tc.feeds...)
			deps := Deps{DB: d}
			cl, err := deps.claimDue(context.Background(), now)
			if err != nil {
				t.Fatal(err)
			}
			var want []int64
			for _, i := range tc.want {
				want = append(want, ids[i])
			}
			slices.Sort(want)
			if !slices.Equal(cl.IDs, want) {
				t.Fatalf("claimed %v, want %v", cl.IDs, want)
			}
			if until := now.Add(syncInterval).Unix(); cl.Until != until {
				t.Errorf("until %d, want %d", cl.Until, until)
			}
			for i, id := range ids {
				var next int64
				var disabled *int64
				if err := d.Reader.QueryRow(`SELECT next_fetch_at, disabled_at FROM feeds WHERE id = ?`, id).Scan(&next, &disabled); err != nil {
					t.Fatal(err)
				}
				claimedIt := slices.Contains(cl.IDs, id)
				if claimedIt && next != cl.Until {
					t.Errorf("claimed feed %d: next_fetch_at %d, want %d", id, next, cl.Until)
				}
				if !claimedIt && next != tc.feeds[i].next {
					t.Errorf("unclaimed feed %d moved: %d", id, next)
				}
			}
		})
	}
}

// A second claim (a retried claim step, or an overlapping cron run) gets
// nothing for feeds the first already moved, so they are sent once. The
// event IDs are fixed by the claim, so a retried send dedups, and the
// feed's next claim gets a new ID.
func TestClaimNoDoubleSend(t *testing.T) {
	d := openDB(t)
	ids := addFeeds(t, d, feedRow{0, nil}, feedRow{0, nil})
	deps := Deps{DB: d}
	first, err := deps.claimDue(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.IDs, ids) {
		t.Fatalf("first claim %v, want %v", first.IDs, ids)
	}
	again, err := deps.claimDue(context.Background(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(again.IDs) != 0 {
		t.Fatalf("second claim %v, want none", again.IDs)
	}

	evs, retry := scheduledSyncs(first), scheduledSyncs(first)
	if len(evs) != 2 || evs[0].ID == nil || *evs[0].ID == *evs[1].ID {
		t.Fatalf("events %+v", evs)
	}
	for i := range evs {
		if *evs[i].ID != *retry[i].ID || evs[i].Name != SyncFeedEvent || evs[i].Data["feed_id"] != ids[i] {
			t.Errorf("event %d: %+v vs retry %+v", i, evs[i], retry[i])
		}
	}

	// Lost events: an hour later the feeds are due again, under new IDs.
	later, err := deps.claimDue(context.Background(), now.Add(syncInterval))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(later.IDs, ids) {
		t.Fatalf("later claim %v, want %v", later.IDs, ids)
	}
	if next := scheduledSyncs(later); *next[0].ID == *evs[0].ID {
		t.Errorf("reclaim reused event ID %s", *next[0].ID)
	}
}

// A feed at a dead URL is claimed whenever due, backs off 30 min doubling
// to the 24 h cap, and the 10th failure disables it, after which it is
// never due.
func TestDeadFeedBacksOffThenDisables(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	backoff := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	at := now
	for i := 1; i <= 10; i++ {
		// Just before it is due, the feed is not claimed.
		if i > 1 {
			if cl, err := e.d.claimDue(context.Background(), at.Add(-time.Second)); err != nil || len(cl.IDs) != 0 {
				t.Fatalf("failure %d: early claim %v, %v", i, cl.IDs, err)
			}
		}
		cl, err := e.d.claimDue(context.Background(), at)
		if err != nil || !slices.Equal(cl.IDs, []int64{e.feed}) {
			t.Fatalf("failure %d: claim %v, %v", i, cl.IDs, err)
		}
		if res := e.sync(t, at); res.Status != "failed" {
			t.Fatalf("failure %d: %+v", i, res)
		}
		f := e.getFeed(t)
		if f.FailCount != int64(i) || f.LastError == "" {
			t.Fatalf("failure %d: feed %+v", i, f)
		}
		if i < 10 {
			if f.DisabledAt.Valid || f.NextFetchAt != at.Add(backoff[i-1]).Unix() {
				t.Fatalf("failure %d: feed %+v, want next at +%v", i, f, backoff[i-1])
			}
			at = time.Unix(f.NextFetchAt, 0)
			continue
		}
		if !f.DisabledAt.Valid || f.DisabledAt.Int64 != at.Unix() {
			t.Fatalf("10th failure did not disable: %+v", f)
		}
	}
	if cl, err := e.d.claimDue(context.Background(), at.Add(365*24*time.Hour)); err != nil || len(cl.IDs) != 0 {
		t.Errorf("disabled feed claimed: %v, %v", cl.IDs, err)
	}
}
