package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/inngest/inngestgo"
	"github.com/inngest/inngestgo/step"

	"github.com/0xD34DC0D3/scry/internal/db"
)

// SyncFeedEvent asks for one feed to be fetched now.
const SyncFeedEvent = "scry/feed.sync"

const (
	syncInterval = time.Hour // after a success, and the schedule's claim lease
	maxLastError = 500       // bytes
)

// SyncFeedData is the event's data.
type SyncFeedData struct {
	FeedID int64 `json:"feed_id"`
}

// SyncFeed builds the event that syncs one feed.
func SyncFeed(feedID int64) inngestgo.Event {
	return inngestgo.Event{Name: SyncFeedEvent, Data: map[string]any{"feed_id": feedID}}
}

// syncResult is the step's output: small, since Inngest stores it.
type syncResult struct {
	Status string `json:"status"` // synced, not_modified, failed, skipped
	Items  int    `json:"items,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (d Deps) registerSyncFeed(c inngestgo.Client) error {
	_, err := inngestgo.CreateFunction(c,
		inngestgo.FunctionOpts{
			ID: "sync-feed", Name: "Sync feed",
			// One sync per feed at a time, so the cron and "sync now"
			// never fetch the same feed at once (extra runs queue), and
			// 5 overall (the Inngest plan's limit; design.md said about 10).
			// The function is one step, so step-level limits cover the whole run.
			Concurrency: &inngestgo.ConfigConcurrency{Step: []inngestgo.ConfigStepConcurrency{
				{Limit: 1, Key: inngestgo.StrPtr("event.data.feed_id")},
				{Limit: 5},
			}},
		},
		inngestgo.EventTrigger(SyncFeedEvent, nil),
		func(ctx context.Context, in inngestgo.Input[SyncFeedData]) (any, error) {
			// One step for fetch, parse and write: a step's output is all
			// that crosses calls, and a feed body is too big to carry.
			// 15 s of fetch plus a short write fits serve's 30 s write
			// timeout.
			return step.Run(ctx, "sync", func(ctx context.Context) (syncResult, error) {
				return d.syncFeed(ctx, in.Event.Data.FeedID, time.Now())
			})
		})
	return err
}

// syncFeed fetches, parses and stores one feed. A failed fetch or parse is
// recorded on the feed and returns no error: retrying it within the run
// would only repeat the request and count one failure several times, and
// the schedule already retries with backoff. Errors returned are the
// database's, which a step retry may well cure.
func (d Deps) syncFeed(ctx context.Context, id int64, now time.Time) (syncResult, error) {
	feed, err := db.New(d.DB.Reader).GetFeed(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return syncResult{Status: "skipped", Error: "no such feed"}, nil
	}
	if err != nil {
		return syncResult{}, err
	}
	if feed.DisabledAt.Valid {
		return syncResult{Status: "skipped", Error: "paused"}, nil
	}

	res, err := d.fetcher.fetch(ctx, feed.Url, feed.Etag, feed.LastModified)
	var parsed parsedFeed // stays empty on a 304: nothing to write
	if err == nil && !res.notModified {
		parsed, err = parseFeed(res.body)
	}
	if err != nil {
		if ctx.Err() != nil {
			// Our own cancellation (shutdown), not the feed's fault.
			return syncResult{}, ctx.Err()
		}
		msg := err.Error()
		if len(msg) > maxLastError {
			msg = strings.ToValidUTF8(msg[:maxLastError], "")
		}
		n, err := db.New(d.DB.Writer).FeedFailed(ctx, db.FeedFailedParams{ID: id, LastError: msg, Now: now.Unix()})
		if err != nil {
			return syncResult{}, err
		}
		if n == 0 {
			return skippedLate, nil
		}
		return syncResult{Status: "failed", Error: msg}, nil
	}
	return d.store(ctx, id, res, parsed, now)
}

// skippedLate is a feed paused or removed while it was being fetched.
var skippedLate = syncResult{Status: "skipped", Error: "paused or removed during fetch"}

// store is the one write transaction for a successful fetch or a 304.
func (d Deps) store(ctx context.Context, id int64, res fetched, p parsedFeed, now time.Time) (syncResult, error) {
	tx, err := d.DB.Writer.BeginTx(ctx, nil)
	if err != nil {
		return syncResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	q := db.New(d.DB.Writer).WithTx(tx)

	n, err := q.FeedSynced(ctx, db.FeedSyncedParams{
		ID: id, Title: p.title, SiteUrl: p.siteURL,
		Etag: res.etag, LastModified: res.lastModified,
		NextFetchAt: now.Add(syncInterval).Unix(),
	})
	if err != nil {
		return syncResult{}, err
	}
	if n == 0 {
		// Before any item: a removed feed's items would fail the foreign
		// key, and a paused one should not change.
		return skippedLate, nil
	}
	for _, it := range p.items {
		var pub sql.NullInt64
		if it.publishedAt != nil {
			pub = sql.NullInt64{Int64: it.publishedAt.Unix(), Valid: true}
		}
		itemID, err := q.UpsertItem(ctx, db.UpsertItemParams{
			FeedID: id, DedupKey: it.dedupKey, Url: it.url, Title: it.title,
			Author: it.author, PublishedAt: pub, FirstSeenAt: now.Unix(),
			CommentsUrl: it.commentsURL,
		})
		if err != nil {
			return syncResult{}, fmt.Errorf("upsert item: %w", err)
		}
		if err := q.UpsertItemContent(ctx, db.UpsertItemContentParams{
			ItemID: itemID, Html: it.html, Excerpt: it.excerpt, FetchedAt: now.Unix(),
		}); err != nil {
			return syncResult{}, fmt.Errorf("upsert content: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return syncResult{}, err
	}
	if res.notModified {
		return syncResult{Status: "not_modified"}, nil
	}
	return syncResult{Status: "synced", Items: len(p.items)}, nil
}
