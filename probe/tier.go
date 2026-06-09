package probe

import "github.com/goceleris/celeris/engine"

// IORING_FEAT_* bits from include/uapi/linux/io_uring.h. Reported via
// the `features` field returned by io_uring_setup(2) — defence-in-depth
// cross-check so a forked / vendor-backported kernel doesn't get gated
// purely on the major.minor pair from `uname`.
//
// We only check the feature bits we depend on; the broader set is
// documented for diagnostics but unused at the gate. Values match the
// kernel headers exactly.
const (
	iouringFeatRsrcTags = 1 << 10 // since 5.13 — sentinel for "real 5.13+ kernel"
	iouringFeatCQESkip  = 1 << 11 // since 5.17 — used to suppress success CQEs (unused at gate; reserved for future cross-check)
)

// determineTier maps a parsed kernel version + the IORING_FEAT_* bits
// reported by io_uring_setup(2)'s sentinel call to a celeris [engine.Tier]
// and the per-feature flags the engine consults. `ops` is the
// IORING_REGISTER_PROBE op support bitmap; reserved for a future
// op-availability cross-check (celeris#287 Finding 4) and ignored
// for now.
//
// Gate-by-gate rationale (all flag/feature versions confirmed against
// io_uring_setup(2) man-page + LWN merge-window articles — see the
// references in celeris#287):
//
//   - 5.10 floor:  the io_uring syscall itself landed in 5.1, but
//     pre-5.10 kernels are non-LTS-stable for io_uring (deadlocks,
//     credential races, registered-fd bugs). 5.10 is the LTS-stable
//     cut-off: Debian 11, RHEL 8.5+, and every distro since carry
//     5.10+. Anything older falls back to epoll.
//   - 5.19 SETUP_COOP_TASKRUN:  the flag did not exist before 5.19.
//     Pre-fix (celeris ≤ v1.4.7), the gate was 5.13, which made every
//     5.13–5.18 kernel (Ubuntu 22.04 LTS / 5.15 included) attempt
//     io_uring_setup with a flag the kernel rejects with -EINVAL,
//     forcing a noisy fall-back to Base. With the gate at 5.19, the
//     5.13–5.18 range simply uses Base without the failed setup probe.
//     The historical `Mid` tier that briefly represented 5.13–5.18 was
//     retired in v1.4.8 (celeris#287) and is gone from this enum.
//   - 5.19 SETUP_SINGLE_ISSUER:  landed in 5.19 alongside COOP_TASKRUN
//     and the multishot / provided-buffer set; treated as one
//     capability tier (High).
//   - 6.0 SETUP_SQPOLL:  the SQPOLL flag itself has been in io_uring
//     since 5.1 (unprivileged from 5.13), but Celeris-supported SQPOLL
//     means SQPOLL ∧ SINGLE_ISSUER — the lock-free SQ batching path
//     that's actually been benchmarked. SQPOLL on its own (without
//     SINGLE_ISSUER) gives a more modest gain not worth the kernel-
//     thread CPU overhead, and unprivileged SQPOLL in the 5.13–5.x
//     range has been the source of several upstream CVE backports.
//     Bundling SQPOLL into the 6.0+ Optional tier keeps the supported
//     configuration sharp.
//   - 6.0 SEND_ZC:  the IORING_OP_SEND_ZC opcode landed in 6.0.
//   - 6.1 SETUP_DEFER_TASKRUN:  landed in 6.1; replaces COOP_TASKRUN
//     in the setup flags on the High and Optional tiers.
//
// Tier topology:
//
//	None     — kernel < 5.10 or io_uring probe failed (engine falls back to epoll).
//	Base     — 5.10 ≤ kernel < 5.19. Linked-SQE chains, single-shot
//	           accept/recv. No COOP_TASKRUN.
//	High     — 5.19 ≤ kernel < 6.0. Adds multishot accept/recv, provided
//	           buffers, fixed files, COOP_TASKRUN, SINGLE_ISSUER.
//	Optional — kernel ≥ 6.0. Adds SQPOLL and SEND_ZC. With kernel ≥ 6.1,
//	           DEFER_TASKRUN replaces COOP_TASKRUN in setup flags.
//
func determineTier(kv KernelVersion, features uint32, _ []uint8) (tier engine.Tier, multishotAccept, multishotRecv, providedBuffers, sqpoll, coopTaskrun, singleIssuer, linkedSQEs, deferTaskrun, fixedFiles, sendZC bool) {
	if !kv.AtLeast(5, 10) {
		return engine.None, false, false, false, false, false, false, false, false, false, false
	}

	// Defence-in-depth: a real ≥ 5.13 kernel reports IORING_FEAT_RSRC_TAGS
	// in `features`. If the kernel claims via uname to be ≥ 5.13 but the
	// bit is missing, treat it as a vendor / forked kernel whose feature
	// surface diverges from the upstream version table and clamp to Base.
	// Conservative — the engine's runtime probe at NewRing time will
	// validate every actually-requested flag anyway, so the worst case
	// is a one-tier downgrade with a clear diagnostic. Issue #287
	// Finding 4.
	versionClaimsAtLeast513 := kv.AtLeast(5, 13)
	featuresConfirmAtLeast513 := features&iouringFeatRsrcTags != 0
	suspectVendorKernel := versionClaimsAtLeast513 && !featuresConfirmAtLeast513

	tier = engine.Base
	linkedSQEs = true

	if kv.AtLeast(5, 19) && !suspectVendorKernel {
		tier = engine.High
		multishotAccept = true
		multishotRecv = true
		providedBuffers = true
		fixedFiles = true
		coopTaskrun = true
		singleIssuer = true
	}

	if kv.AtLeast(6, 0) && !suspectVendorKernel {
		tier = engine.Optional
		sqpoll = true
		sendZC = true
	}

	if kv.AtLeast(6, 1) && !suspectVendorKernel {
		deferTaskrun = true
	}

	return
}
