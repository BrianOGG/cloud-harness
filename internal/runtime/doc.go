// Package runtime holds scheduler policy that is not itself a persisted fact.
// The legal run and invocation transitions live in domain so every adapter
// shares one table. Lease duration lives here. The SQLite store is the lease
// and budget ledger the manager actually locks.
package runtime

import "time"

// LeaseTTL is how long a run owner may dispatch before it must heartbeat.
const LeaseTTL = 30 * time.Second
