package db

import (
	"context"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/0xD34DC0D3/scry/migrations"
)

// The public lists rely on the posted_at expression index (migration 00006),
// which SQLite uses only when a query spells the expression exactly as the
// index does and compares it without an affinity conversion. Either slip
// still returns the right rows, just by scanning, so only the plan shows it.
func TestPublicQueriesUseIndex(t *testing.T) {
	d := openTest(t)
	p, err := goose.NewProvider(goose.DialectSQLite3, d.Writer, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, query string
		args        int
		want        string
		sorted      bool // rows must come out of the index in order
	}{
		{"latest", listLatestItems, 4, "SEARCH i USING INDEX items_posted (<expr>>? AND <expr><?)", true},
		{"feed", listFeedItems, 5, "SEARCH i USING INDEX items_feed_posted (feed_id=? AND <expr>>? AND <expr><?)", true},
		{"catalog count", listCatalog, 1, "SEARCH i USING COVERING INDEX items_feed_posted (feed_id=? AND <expr>>?)", false},
		// max of posted over feed_id=? on an index ending in posted: SQLite
		// seeks the feed's last entry instead of reading all its items.
		{"catalog last post", listCatalog, 1, "SEARCH i USING COVERING INDEX items_feed_posted (feed_id=?)\n", false},
		{"feed last post", getFeedLastPostAt, 1, "SEARCH i USING COVERING INDEX items_feed_posted (feed_id=?)", false},
		{"latest count", countLatestItems, 3, "SEARCH i USING COVERING INDEX items_posted (<expr>>?)", false},
		{"feed count", countFeedItems, 4, "SEARCH i USING COVERING INDEX items_feed_posted (feed_id=? AND <expr>>?)", false},
		// Reading (step 6): per subscribed feed, the posted range of that
		// feed; the merge across feeds sorts (a page of 10, after the range).
		{"reading list", listReadingItems, 7, "SEARCH i USING INDEX items_feed_posted (feed_id=? AND <expr>>? AND <expr><?)", false},
		// A feed's page for a signed-in non-subscriber: the page comes off
		// the feed's posted range, and item_contents is read by key for
		// that page only.
		{"feed posts", listFeedPosts, 5, "SEARCH i USING INDEX items_feed_posted (feed_id=? AND <expr>>? AND <expr><?)", true},
		{"feed posts content", listFeedPosts, 5, "SEARCH c USING INTEGER PRIMARY KEY (rowid=?)", false},
		{"reading unread count", listSubscriptions, 2, "SEARCH i USING INDEX items_feed_posted (feed_id=? AND <expr>>?)", false},
		// Mark all read: the newest pre-render first_seen_at of each feed,
		// read backwards from the end of items(feed_id, first_seen_at).
		{"mark all read", markAllRead, 3, "SEARCH i USING COVERING INDEX items_feed_first_seen (feed_id=?)", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := queryPlan(t, d, tc.query, tc.args)
			if !strings.Contains(got, tc.want) {
				t.Errorf("plan lacks %q:\n%s", tc.want, got)
			}
			if tc.sorted && strings.Contains(got, "TEMP B-TREE") {
				t.Errorf("plan sorts:\n%s", got)
			}
		})
	}
}

func queryPlan(t *testing.T, d *DB, query string, nargs int) string {
	t.Helper()
	args := make([]any, nargs)
	for i := range args {
		args[i] = int64(1)
	}
	rows, err := d.Reader.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}
