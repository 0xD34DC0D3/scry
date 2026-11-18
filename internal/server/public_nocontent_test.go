package server

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Pruning deletes item_contents but keeps items, so an item can lack a
// content row; it must still be listed with an empty excerpt, never hidden.
func TestPublicItemWithoutContent(t *testing.T) {
	f := newPublicFixture(t)
	id := seedItem(t, f.d, f.a.ID, "nc", "https://a.example/nc", "Alpha pruned", "gone excerpt", 30*time.Minute)
	if _, err := f.d.Writer.Exec(`DELETE FROM item_contents WHERE item_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/feeds/" + strconv.FormatInt(f.a.ID, 10)} {
		t.Run(path, func(t *testing.T) {
			rec := do(f.h, get(path, "", nil))
			body := rec.Body.String()
			if !strings.Contains(body, "Alpha pruned") || strings.Contains(body, "gone excerpt") {
				t.Fatalf("item missing or excerpt kept: %d %s", rec.Code, body)
			}
		})
	}
}
