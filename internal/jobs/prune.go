package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/inngest/inngestgo"
	"github.com/inngest/inngestgo/step"

	"github.com/0xD34DC0D3/scry/internal/db"
)

// Daily at 03:17 UTC, off the hour so it rarely lines up with other jobs.
// Rows older than retention go, 500 per batch, one step per batch. A batch
// is one short write on the single Writer (500 rows is tens of ms even with
// large html), so syncs wait at most that long and each step stays far
// inside serve's 30 s write timeout. At most maxBatches steps per table per
// run (about 100 + 100 + 1 steps, far under Inngest's ~1000 per run); a
// backlog bigger than 50,000 rows per table finishes on the next days'
// runs. Steady state is one day's worth of rows, a few batches.
const (
	pruneCron  = "17 3 * * *"
	pruneBatch = 500
	maxBatches = 100
	// retention matches the 30-day window of the views (server.window): a
	// row pruned here belongs to an item the views no longer show.
	retention = 30 * 24 * time.Hour
)

func (d Deps) registerPrune(c inngestgo.Client) error {
	_, err := inngestgo.CreateFunction(c,
		inngestgo.FunctionOpts{ID: "prune", Name: "Prune old content and reads"},
		inngestgo.CronTrigger(pruneCron),
		func(ctx context.Context, _ inngestgo.Input[any]) (any, error) {
			// One cutoff for the whole run, memoized by its step: Inngest
			// replays this function body for every step, and a clock read
			// here would differ between replays.
			cutoff, err := step.Run(ctx, "cutoff", func(context.Context) (int64, error) {
				return time.Now().Add(-retention).Unix(), nil
			})
			if err != nil {
				return nil, err
			}
			var total pruned
			for _, t := range []struct {
				name string
				fn   func(context.Context, int64) (int64, error)
			}{
				{"contents", d.pruneContents},
				{"reads", d.pruneReads},
			} {
				for i := 0; i < maxBatches; i++ {
					n, err := step.Run(ctx, fmt.Sprintf("prune-%s-%d", t.name, i), func(ctx context.Context) (int64, error) {
						return t.fn(ctx, cutoff)
					})
					if err != nil {
						return nil, err
					}
					total.add(t.name, n)
					if n < pruneBatch {
						break
					}
				}
			}
			return total, nil
		})
	return err
}

// pruned counts rows deleted by a run.
type pruned struct {
	Contents int64 `json:"contents"`
	Reads    int64 `json:"reads"`
}

func (p *pruned) add(name string, n int64) {
	if name == "contents" {
		p.Contents += n
	} else {
		p.Reads += n
	}
}

// pruneContents deletes up to pruneBatch item_contents fetched before
// cutoff (unix seconds) and returns how many. A retry after a committed
// delete just deletes the next batch, so steps are safe to repeat.
func (d Deps) pruneContents(ctx context.Context, cutoff int64) (int64, error) {
	return db.New(d.DB.Writer).PruneItemContents(ctx, db.PruneItemContentsParams{Cutoff: cutoff, Lim: pruneBatch})
}

// pruneReads deletes up to pruneBatch item_reads read before cutoff.
func (d Deps) pruneReads(ctx context.Context, cutoff int64) (int64, error) {
	return db.New(d.DB.Writer).PruneItemReads(ctx, db.PruneItemReadsParams{Cutoff: cutoff, Lim: pruneBatch})
}
