//go:build linux

package iouring

// The scaler algorithm itself lives in engine/scaler and has its own
// test suite there. The iouring engine's scaler.go is just the
// engine.WorkerScaler adapter (PauseWorker / ResumeWorker on workers
// slice). The runtime exercises this adapter via the existing engine
// integration tests (e.g., async_churn_uaf_test.go) and the spike-B
// strict-matrix runs that ran with CELERIS_DYN_WORKERS=1.
