package mono

// The table of TestNoWallStamps (see the package documentation): the wall reads
// that stay wall for good. The pending tables of the routing steps are gone: every
// stamp of the engines, conn, the root package and the HTTP/2 stream code is read
// from this package (celeris#1178), and a new wall read has to be argued for here.

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
