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
	{"context.go", "Context.StartTime", "time.Unix"}:                    {1, "step 4"},
	{"context_response.go", "Context.Detach", ".UnixNano"}:              {1, "step 4"},
	{"handler.go", "routerAdapter.HandleStream", ".UnixNano"}:           {1, "step 4"},
	{"handler.go", "routerAdapter.recoverAndRelease", ".UnixNano"}:      {1, "step 4"},
	{"internal/conn/h1.go", "H1State.ArmHeaderDeadline", ".UnixNano"}:   {1, "step 4"},
	{"internal/conn/h1.go", "H1State.ArmHeaderDeadlineAt", ".UnixNano"}: {1, "step 4"},
	// step 5: route the HTTP/2 stream code (internal/protocol/h2/stream)
	{"internal/protocol/h2/stream/deadline.go", "Processor.armHeaderBlock", ".UnixNano"}:     {1, "step 5"},
	{"internal/protocol/h2/stream/processor.go", "Processor.admitHeaders", ".UnixNano"}:      {1, "step 5"},
	{"internal/protocol/h2/stream/write_deadline.go", "Manager.noteBuffered", ".UnixNano"}:   {1, "step 5"},
	{"internal/protocol/h2/stream/write_wait.go", "Manager.beginWait", ".UnixNano"}:          {2, "step 5"},
	{"internal/protocol/h2/stream/write_wait.go", "Manager.judgeWait", ".UnixNano"}:          {1, "step 5"},
	{"internal/protocol/h2/stream/write_wait.go", "Stream.awaitSendWindowRate", "time.Unix"}: {2, "step 5"},
}

// pendingFixtures: test functions that write a wall reading into a stamp field or
// hand one to a function that takes a stamp. Each moves to mono.Now() with the
// routing PR of its package (the skew canary fails them otherwise), and its entry
// goes with it.
var pendingFixtures = map[string]bool{
	// step 4 (root package)
	"detach_elapsed_889_test.go | TestDetachSnapshotElapsedFromStartTimeNs889": true,
	// step 4
	"internal/conn/h1_header_deadline_at_test.go | TestArmHeaderDeadlineAt":                  true,
	"internal/conn/h2_sweep_889_test.go | TestSweepAnswersABurstOfExpiriesInOneSweep889":     true,
	"internal/conn/h2_sweep_889_test.go | TestSweepExpiresARequestAnswers408AndResets889":    true,
	"internal/conn/h2_sweep_889_test.go | TestSweepExtendIdleAndGoAwayNoWait889":             true,
	"internal/conn/h2_sweep_889_test.go | TestSweepExtendIdleNeedsProgress889":               true,
	"internal/conn/h2_sweep_889_test.go | TestSweepFramesOnAResetStreamAreNotActivity889":    true,
	"internal/conn/h2_sweep_889_test.go | TestSweepHeaderBlockDeadlineEndsTheConn889":        true,
	"internal/conn/h2_sweep_889_test.go | TestSweepIdleClockIgnoresPings889":                 true,
	"internal/conn/h2_sweep_889_test.go | TestSweepResetsAStalledStreamWithInternalError889": true,
	"internal/conn/h2_sweep_889_test.go | TestSweepTakesNoLockWhenNothingIsDue889":           true,
	// step 5
	"internal/protocol/h2/stream/request_deadline_889_test.go | TestClientResetOfAnExpiredRequestForgetsIt889":        true,
	"internal/protocol/h2/stream/request_deadline_889_test.go | TestEveryRequestOpenStreamCarriesADeadline889":        true,
	"internal/protocol/h2/stream/request_deadline_889_test.go | TestExpireRequestAnswers408AndRemembersTheStream889":  true,
	"internal/protocol/h2/stream/request_deadline_889_test.go | TestExpiredRequestTombstonesRunOut889":                true,
	"internal/protocol/h2/stream/request_deadline_889_test.go | TestRequestDeadlineDisarmedWhenTheStreamLeaves889":    true,
	"internal/protocol/h2/stream/request_deadline_889_test.go | TestRequestDeadlineNeverAllocatesOnTheRequestPath889": true,
	"internal/protocol/h2/stream/write_deadline_889_test.go | TestExpiredWritesBatch889":                              true,
	"internal/protocol/h2/stream/write_deadline_889_test.go | TestExpiredWritesWindows889":                            true,
	"internal/protocol/h2/stream/write_rate_889_test.go | TestExpiredWritesJudgesTheRecordItFoundDue889":              true,
	"internal/protocol/h2/stream/write_rate_889_test.go | TestHugeTimeoutsDoNotOverflow889":                           true,
	"internal/protocol/h2/stream/write_wait_1170_test.go | TestWaiterAndBufferNeverHaveTwoJudges1170":                 true,
	"internal/protocol/h2/stream/write_wait_1170_test.go | TestWaiterRecordBooks1170":                                 true,
}
