package jobs

import "testing"

// Registration validates function configs (IDs, cron syntax) up front.
func TestNew(t *testing.T) {
	t.Setenv("INNGEST_DEV", "1")
	if _, err := New(Deps{}); err != nil {
		t.Fatal(err)
	}
}
