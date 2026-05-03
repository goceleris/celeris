//go:build linux

package epoll

// The scaler algorithm itself lives in engine/scaler and has its own
// test suite there. The epoll engine's scaler.go is just the
// engine.WorkerScaler adapter (PauseWorker / ResumeWorker on loops
// slice).
