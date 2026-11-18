package jobs

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/inngest/inngestgo"
	"github.com/inngest/inngestgo/step"

	"github.com/0xD34DC0D3/scry/internal/db"
)

// Every 10 minutes, at most 100 feeds. A feed is synced at most 10 min
// late, which is fine for hourly feeds, and an idle tick is one cheap step.
// 100 syncs drain in under 3 min at 10 concurrent 15 s fetches, well
// inside a tick; more than 100 due wait for the next tick, oldest first.
// The claim is one UPDATE and the send one API call, each far inside
// serve's 30 s write timeout, and the claim's output (100 ids) stays small.
const (
	scheduleCron = "*/10 * * * *"
	claimBatch   = 100
)

// claimed is the claim step's output.
type claimed struct {
	IDs   []int64 `json:"ids,omitempty"`
	Until int64   `json:"until,omitempty"` // the next_fetch_at the claim set
}

func (d Deps) registerSchedule(c inngestgo.Client) error {
	_, err := inngestgo.CreateFunction(c,
		inngestgo.FunctionOpts{ID: "schedule-feeds", Name: "Schedule feed syncs"},
		inngestgo.CronTrigger(scheduleCron),
		func(ctx context.Context, _ inngestgo.Input[any]) (any, error) {
			// The claim is memoized once Inngest records it, so a retried
			// send step reuses these ids. A claim retried after its
			// UPDATE committed finds those feeds already moved and
			// returns nothing for them: their events are lost, and they
			// come due again at Until.
			cl, err := step.Run(ctx, "claim", func(ctx context.Context) (claimed, error) {
				return d.claimDue(ctx, time.Now())
			})
			if err != nil || len(cl.IDs) == 0 {
				return 0, err
			}
			if _, err := step.SendMany(ctx, "send", scheduledSyncs(cl)); err != nil {
				return nil, err
			}
			return len(cl.IDs), nil
		})
	return err
}

// claimDue moves up to claimBatch due feeds' next_fetch_at forward by
// syncInterval and returns their ids. If a sync never runs (lost event),
// the feed is simply due again then; a sync that runs sets its own next
// time.
func (d Deps) claimDue(ctx context.Context, now time.Time) (claimed, error) {
	until := now.Add(syncInterval).Unix()
	ids, err := db.New(d.DB.Writer).ClaimDueFeeds(ctx, db.ClaimDueFeedsParams{
		Until: until, Now: now.Unix(), Lim: claimBatch,
	})
	if err != nil || len(ids) == 0 {
		return claimed{}, err
	}
	slices.Sort(ids) // RETURNING order is unspecified
	return claimed{IDs: ids, Until: until}, nil
}

// scheduledSyncs builds one event per claimed feed. The ID is fixed by the
// claim (feed id and the time it was moved to), so a retried send step is
// deduplicated by Inngest, while the next claim of the same feed gets a
// new ID.
func scheduledSyncs(cl claimed) []inngestgo.Event {
	evs := make([]inngestgo.Event, len(cl.IDs))
	for i, id := range cl.IDs {
		ev := SyncFeed(id)
		ev.ID = inngestgo.StrPtr(fmt.Sprintf("scry-feed-sync-%d-%d", id, cl.Until))
		evs[i] = ev
	}
	return evs
}
