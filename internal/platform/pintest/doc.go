// Package pintest holds what the celeris#905 regression tests share: a census
// of the CPU affinity of every thread in this process, a probe of the mask a
// goroutine sees when it locks an OS thread, and the re-exec that runs a test
// alone in a process of its own. It is for tests only, and Linux only: the
// affinity it reads is /proc/self/task/*/status.
package pintest
