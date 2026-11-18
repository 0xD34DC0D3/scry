// Package jobs creates scry's Inngest client and registers its functions.
// Inngest calls them over HTTP at Path; the client also sends events.
//
// The SDK reads its own env vars (INNGEST_SIGNING_KEY, INNGEST_EVENT_KEY,
// INNGEST_DEV, ...); internal/config reads the same ones only to refuse
// unsafe prod settings, so there is one name for each setting.
package jobs

import (
	"log/slog"

	"github.com/inngest/inngestgo"

	"github.com/0xD34DC0D3/scry/internal/db"
)

// Path is where the SDK's handler is mounted. The dev server and Inngest
// Cloud are pointed at it.
const Path = "/api/inngest"

// MaxBody caps a request from Inngest. A call carries the event plus every
// earlier step's output, and Inngest caps a run's whole state at 32 MiB
// (https://www.inngest.com/docs/usage-limits/inngest); the extra 4 MiB is
// headroom for the JSON envelope around it.
const MaxBody = 36 << 20

// Deps is everything the functions use. Passed in, never global, so tests
// can hand them a temp DB.
type Deps struct {
	DB *db.DB // writes go through DB.Writer, the single writer

	fetcher *fetcher // New sets the real one; tests in this package set their own
}

// New returns the client with every function registered.
func New(d Deps) (inngestgo.Client, error) {
	c, err := inngestgo.NewClient(inngestgo.ClientOpts{
		AppID: "scry",
		// The SDK's own cap (default 100 MB) is kept in step with the
		// server's, so neither silently undercuts the other.
		MaxBodySize: MaxBody,
		Logger:      slog.Default(),
	})
	if err != nil {
		return nil, err
	}
	if d.fetcher == nil {
		d.fetcher = newFetcher()
	}
	if err := d.register(c); err != nil {
		return nil, err
	}
	return c, nil
}

// register adds every function to c.
func (d Deps) register(c inngestgo.Client) error {
	return d.registerSyncFeed(c)
}
