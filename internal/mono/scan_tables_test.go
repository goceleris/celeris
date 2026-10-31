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
	// step 3: route the epoll engine (internal/engine/epoll)
	{"internal/engine/epoll/loop.go", "Loop.acceptQueuedOnPause", ".UnixNano"}:       {1, "step 3"},
	{"internal/engine/epoll/loop.go", "Loop.armEpollOut", ".UnixNano"}:               {1, "step 3"},
	{"internal/engine/epoll/loop.go", "Loop.checkTimeouts", ".UnixNano"}:             {1, "step 3"},
	{"internal/engine/epoll/loop.go", "Loop.handleWritable", ".UnixNano"}:            {1, "step 3"},
	{"internal/engine/epoll/loop.go", "Loop.markClosing", ".UnixNano"}:               {1, "step 3"},
	{"internal/engine/epoll/loop.go", "Loop.run", ".UnixNano"}:                       {4, "step 3"},
	{"internal/engine/epoll/loop.go", "Loop.shutdown", ".UnixNano"}:                  {2, "step 3"},
	{"internal/engine/epoll/loop.go", "connState.endWriteSlow", ".UnixNano"}:         {1, "step 3"},
	{"internal/engine/epoll/loop.go", "connState.noteClosingProgress", ".UnixNano"}:  {1, "step 3"},
	{"internal/engine/epoll/loop.go", "connState.noteWriteProgress", ".UnixNano"}:    {1, "step 3"},
	{"internal/engine/epoll/poolwait_gate_885.go", "Loop.poolWaitGate", ".UnixNano"}: {1, "step 3"},
	{"internal/engine/epoll/sweep.go", "Loop.sweep", ".UnixNano"}:                    {1, "step 3"},
	{"internal/engine/epoll/sweep.go", "Loop.sweepWaitMs", ".UnixNano"}:              {1, "step 3"},
	{"internal/engine/epoll/transplant.go", "Loop.reclaimTransplant", ".UnixNano"}:   {1, "step 3"},
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
	// step 3
	"internal/engine/epoll/async_abort_844_engine_linux_test.go | TestAbortAfterPeerCloseHandsTheConnBack844":                            true,
	"internal/engine/epoll/async_handler_stall_linux_test.go | TestADeferredTransplantFinishesOnlyAfterItsGoroutineExits":                true,
	"internal/engine/epoll/async_handler_stall_linux_test.go | TestATransplantWaitsForARelink":                                           true,
	"internal/engine/epoll/async_handler_stall_linux_test.go | TestPeerCloseDoesNotWaitForARunningAsyncHandler":                          true,
	"internal/engine/epoll/checktimeouts_floor_885_linux_test.go | TestDetachedIdleDeadlineIsReapedPromptlyUnderLoad885":                 true,
	"internal/engine/epoll/checktimeouts_h2_bench_889_linux_test.go | BenchmarkCheckTimeoutsH2889":                                       true,
	"internal/engine/epoll/closing_drain_clock_876_linux_test.go | TestClosingConnIsNotReapedFromItsLastRead":                            true,
	"internal/engine/epoll/closing_drain_clock_876_linux_test.go | TestPeerHalfClosedConnIsNotReapedFromItsLastRead":                     true,
	"internal/engine/epoll/closing_kernel_progress_876_linux_test.go | TestClosingConnFirstMetAfterItsBoundIsGivenOneMoreBound":          true,
	"internal/engine/epoll/closing_kernel_progress_876_linux_test.go | TestClosingConnIsSampledBeforeItIsReapedWhateverTheSampleRate":    true,
	"internal/engine/epoll/closing_restamp_876_linux_test.go | TestRepeatedCloseRequestsDoNotRestartTheDrainClock":                       true,
	"internal/engine/epoll/detach_reap_drain_linux_test.go | TestDetachedIdleReapDrainIsBounded":                                         true,
	"internal/engine/epoll/detach_stall_888_linux_test.go | TestDetachedPeerThatTakesNothingIsClosed888":                                 true,
	"internal/engine/epoll/detach_stall_888_linux_test.go | TestDetachedReadingPeerIsNotClosed888":                                       true,
	"internal/engine/epoll/detach_stall_888_linux_test.go | TestDetachedStallIsBoundedWhateverTheIdleDeadlineSays888":                    true,
	"internal/engine/epoll/detached_epollout_885_linux_test.go | TestDrainReadsInlineFlushHandsADetachedConnToEPOLLOUT885":               true,
	"internal/engine/epoll/drainread_budget_885_linux_test.go | TestDrainReadBudgetAfterADirectBody885":                                  true,
	"internal/engine/epoll/drainread_budget_885_linux_test.go | TestDrainReadBudgetGivesTheConnAnotherTurn885":                           true,
	"internal/engine/epoll/drainread_budget_885_linux_test.go | TestDrainReadBudgetKeepsEPOLLOUT885":                                     true,
	"internal/engine/epoll/drainread_budget_885_linux_test.go | TestDrainReadBudgetSparesAClosingConn885":                                true,
	"internal/engine/epoll/drainread_budget_885_linux_test.go | pumpReads885":                                                            true,
	"internal/engine/epoll/drainread_budget_bench_885_linux_test.go | BenchmarkDrainReadLoop885":                                         true,
	"internal/engine/epoll/drainread_flood_885_linux_test.go | TestDrainReadIsBoundedUnderAFlood885":                                     true,
	"internal/engine/epoll/gate_promoted_1165_linux_test.go | TestGateClosesAPromotedConnOnlyWhenItIsIdle1165":                           true,
	"internal/engine/epoll/gate_promoted_1165_linux_test.go | TestGateHoldsAPromotedConnWithAnUnreadRequest1165":                         true,
	"internal/engine/epoll/hijack_closeconn_race_linux_test.go | TestCloseConnLeavesAReissuedSlotAloneAfterWaitingOnDetachMu":            true,
	"internal/engine/epoll/hijack_closeconn_race_linux_test.go | hijackRaceConn":                                                         true,
	"internal/engine/epoll/hijack_flush_936_linux_test.go | TestFlushBeforeHijackKeepsTheWriteClock936":                                  true,
	"internal/engine/epoll/hijack_offthread_linux_test.go | TestHijackedEntryDoesNotAliasTheNextOwnerOfItsDescriptor":                    true,
	"internal/engine/epoll/hijack_offthread_linux_test.go | acceptAfterHijackAttempt":                                                    true,
	"internal/engine/epoll/hijack_offthread_linux_test.go | offThreadRig":                                                                true,
	"internal/engine/epoll/http_fd_identity_885_linux_test.go | TestHTTPEventsCarryAGeneration885":                                       true,
	"internal/engine/epoll/http_fd_identity_885_linux_test.go | TestStaleHTTPEventDoesNotReachConnOnReusedNumber885":                     true,
	"internal/engine/epoll/http_fd_identity_885_linux_test.go | adoptedPair885":                                                          true,
	"internal/engine/epoll/poolwait_gate_unread_885_linux_test.go | TestPoolWaitGateDoesNotCloseAConnWithAnUnreadRequest1104":            true,
	"internal/engine/epoll/reclaim_reqcount_886_linux_test.go | TestReclaimTransplantDoesNotCountTheCarriedRecvTwice886":                 true,
	"internal/engine/epoll/review_v150_test.go | TestAcceptAllDrainsBacklogNoStrand":                                                     true,
	"internal/engine/epoll/review_v150_test.go | TestCheckTimeoutsClosesAllExpired":                                                      true,
	"internal/engine/epoll/shutdown_idle_before_drain_1104_linux_test.go | TestShutdownIdlePassTearsAConnThatServedARequestDownOnce1104": true,
	"internal/engine/epoll/sweep_h2_bench_889_linux_test.go | BenchmarkSweepH2Deadlines889":                                              true,
	"internal/engine/epoll/transplant_accounting_test.go | TestAttachAdoptedFDCountsAnOccupiedSlot":                                      true,
	"internal/engine/epoll/transplant_accounting_test.go | TestAttachAdoptedFDCountsTheAdopt":                                            true,
	"internal/engine/epoll/write_clock_885_linux_test.go | TestFlushDirtyRestartsTheWriteClockOnlyWhenItTakesBytes885":                   true,
	"internal/engine/epoll/write_clock_885_linux_test.go | TestReadTimeoutOfAParkedConnRunsFromTheWriteClock885":                         true,
	"internal/engine/epoll/write_clock_885_linux_test.go | TestWriteClockEndsAtTheDrainAndFoldsIntoLastActivity885":                      true,
	"internal/engine/epoll/write_clock_885_linux_test.go | parked885":                                                                    true,
	"internal/engine/epoll/write_clock_armed_885_linux_test.go | TestPartialFlushAfterADrainThatLeftEPOLLOUTArmedStartsTheClock885":      true,
	"internal/engine/epoll/write_clock_armed_885_linux_test.go | TestPartialFlushAfterTheSweepEndedTheClockStartsTheClock885":            true,
	"internal/engine/epoll/write_clock_bench_885_linux_test.go | BenchmarkCheckTimeoutsParked885":                                        true,
	"internal/engine/epoll/write_clock_goroutine_885_linux_test.go | TestGoroutineFlushThatTakesBytesRestartsTheWriteClock885":           true,
	"internal/engine/epoll/write_clock_goroutine_885_linux_test.go | TestGoroutineFlushThatTakesNothingDoesNotRestartTheWriteClock885":   true,
	"internal/engine/epoll/write_clock_sites_885_linux_test.go | TestWriteClockIsNotInheritedByTheNextPark885":                           true,
	"internal/engine/epoll/write_clock_sites_885_linux_test.go | newSitesConn885":                                                        true,
	"internal/engine/epoll/write_clock_sites_885_linux_test.go | sitesConn885.send":                                                      true,
	"internal/engine/epoll/write_rate_rtit_885_linux_test.go | TestParkedConnWithNothingParkedIsStillReapedByReadTimeout885":             true,
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
