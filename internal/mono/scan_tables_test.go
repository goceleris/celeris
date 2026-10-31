package mono

// The two tables of TestNoWallStamps and the list of TestNoWallFixtures (see the
// package documentation). Generated once by TestScanDump; edited by hand since.

type entry struct {
	Count  int
	Reason string
}

// stays: wall reads that stay wall for good, each with its reason.
var stays = map[site]entry{
	{"context_response.go", "Context.SetCookie", ".UTC"}:                               {1, "a cookie's Expires is the user's wall time, rendered into a header"},
	{"internal/conn/response.go", "appendCachedDate", ".UTC"}:                          {1, "the Date header: a wall time, rendered into the response"},
	{"internal/conn/response.go", "updateCachedDate", ".UTC"}:                          {1, "the Date header: a wall time, rendered into the response"},
	{"internal/protocol/h2/stream/processor.go", "Processor.countRefusal", ".Unix"}:    {1, "one-second flood window: equality of the wall second, stamped and compared in this function"},
	{"internal/protocol/h2/stream/processor.go", "Processor.handleRSTStream", ".Unix"}: {1, "one-second RST_STREAM flood window: equality of the wall second, stamped and compared in this function"},
	{"middleware/websocket/conn.go", "Conn.GracefulClose", ".UnixNano"}:                {1, "the public Context.SetWSIdleDeadline contract (wall Unix nanoseconds); the engine converts it with mono.FromWall"},
	{"middleware/websocket/conn.go", "Conn.ReadMessageReuse", ".UnixNano"}:             {1, "the public Context.SetWSIdleDeadline contract (wall Unix nanoseconds); the engine converts it with mono.FromWall"},
	{"router.go", "var nowNano", ".UnixNano"}:                                          {1, "the adaptive promotion TTL: stamped and compared in router.go, with its own test hook"},
}

// pending: every wall stamp that has still to move onto mono.Now(). A routing PR
// deletes its entries; the flip PR requires the table to be empty.
var pending = map[site]entry{
	// step 4: route internal/conn H1 and the root package (handler.go, context_response.go; context.go StartTime keeps time.Unix(0, ns) until the flip PR, which makes it mono.Time and deletes its entry)
	{"context.go", "Context.StartTime", "time.Unix"}: {1, "step 4"},
}

// pendingFixtures: test functions that write a wall reading into a stamp field or
// hand one to a function that takes a stamp. Each moves to mono.Now() with the
// routing PR of its package (the skew canary fails them otherwise), and its entry
// goes with it.
var pendingFixtures = map[string]bool{}
