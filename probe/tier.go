package probe

import "github.com/goceleris/celeris/engine"

func determineTier(kv KernelVersion, _ uint32, _ []uint8) (tier engine.Tier, multishotAccept, multishotRecv, providedBuffers, sqpoll, coopTaskrun, singleIssuer, linkedSQEs bool) {
	if !kv.AtLeast(5, 10) {
		return engine.None, false, false, false, false, false, false, false
	}

	tier = engine.Base
	linkedSQEs = true

	if kv.AtLeast(5, 13) {
		tier = engine.Mid
		coopTaskrun = true
	}

	if kv.AtLeast(5, 19) {
		tier = engine.High
		multishotAccept = true
		multishotRecv = true
		providedBuffers = true
	}

	if kv.AtLeast(6, 0) {
		tier = engine.Optional
		singleIssuer = true
		sqpoll = true
	}

	return
}
