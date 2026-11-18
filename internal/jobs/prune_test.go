package jobs

import (
	"context"
	"fmt"
	"testing"
)

func TestPrune(t *testing.T) {
	cutoff := now.Add(-retention).Unix()
	for _, tc := range []struct {
		name    string
		table   string // item_contents or item_reads
		old     int    // rows older than the cutoff
		batches []int64
	}{
		{"contents none old", "item_contents", 0, []int64{0}},
		{"contents under one batch", "item_contents", 7, []int64{7, 0}},
		{"contents exact batch", "item_contents", pruneBatch, []int64{pruneBatch, 0}},
		{"contents several batches", "item_contents", 2*pruneBatch + 7, []int64{pruneBatch, pruneBatch, 7, 0}},
		{"reads none old", "item_reads", 0, []int64{0}},
		{"reads several batches", "item_reads", 2*pruneBatch + 7, []int64{pruneBatch, pruneBatch, 7, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openDB(t)
			deps := Deps{DB: d}
			feed := addFeeds(t, d, feedRow{})[0]
			ins, col, run := `INSERT INTO item_contents (item_id, html, excerpt, fetched_at) VALUES (?, '', '', ?)`, "fetched_at", deps.pruneContents
			if tc.table == "item_reads" {
				ins, col, run = `INSERT INTO item_reads (user_id, item_id, read_at) VALUES ('u', ?, ?)`, "read_at", deps.pruneReads
			}
			// Old rows, then one exactly at the cutoff (kept: the test is
			// strictly older), one just after and one new. One row per item.
			times := make([]int64, 0, tc.old+3)
			for i := 0; i < tc.old; i++ {
				times = append(times, cutoff-1-int64(i)*3600)
			}
			times = append(times, cutoff, cutoff+1, now.Unix())
			for i, ts := range times {
				res, err := d.Writer.Exec(`INSERT INTO items (feed_id, dedup_key, first_seen_at) VALUES (?, ?, 0)`, feed, fmt.Sprint(i))
				if err != nil {
					t.Fatal(err)
				}
				id, _ := res.LastInsertId()
				if _, err := d.Writer.Exec(ins, id, ts); err != nil {
					t.Fatal(err)
				}
			}
			for i, want := range tc.batches {
				n, err := run(context.Background(), cutoff)
				if err != nil {
					t.Fatal(err)
				}
				if n != want {
					t.Fatalf("batch %d deleted %d, want %d", i, n, want)
				}
			}
			var left, oldLeft, items int
			q := fmt.Sprintf(`SELECT count(*), coalesce(sum(%s < ?), 0) FROM %s`, col, tc.table)
			if err := d.Reader.QueryRow(q, cutoff).Scan(&left, &oldLeft); err != nil {
				t.Fatal(err)
			}
			if left != 3 || oldLeft != 0 {
				t.Errorf("%d rows left, %d old; want 3 and 0", left, oldLeft)
			}
			// Items are kept forever.
			if err := d.Reader.QueryRow(`SELECT count(*) FROM items`).Scan(&items); err != nil || items != len(times) {
				t.Errorf("items %d (err %v), want %d", items, err, len(times))
			}
		})
	}
}

// Each table is judged by its own time column only.
func TestPruneIndependentTables(t *testing.T) {
	d := openDB(t)
	deps := Deps{DB: d}
	feed := addFeeds(t, d, feedRow{})[0]
	cutoff := now.Add(-retention).Unix()
	res, err := d.Writer.Exec(`INSERT INTO items (feed_id, dedup_key, first_seen_at) VALUES (?, 'x', 0)`, feed)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := d.Writer.Exec(`INSERT INTO item_contents (item_id, html, excerpt, fetched_at) VALUES (?, '', '', ?)`, id, cutoff-1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Writer.Exec(`INSERT INTO item_reads (user_id, item_id, read_at) VALUES ('u', ?, ?)`, id, cutoff+1); err != nil {
		t.Fatal(err)
	}
	if n, err := deps.pruneContents(context.Background(), cutoff); err != nil || n != 1 {
		t.Fatalf("contents %d, %v", n, err)
	}
	if n, err := deps.pruneReads(context.Background(), cutoff); err != nil || n != 0 {
		t.Fatalf("reads %d, %v", n, err)
	}
}
