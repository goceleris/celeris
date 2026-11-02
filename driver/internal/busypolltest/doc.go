// Package busypolltest lets a driver's test start a celeris server with a
// chosen Config.SocketBusyPoll and read SO_BUSY_POLL off a descriptor the
// driver dialed, so that a test can say what the driver put on its socket
// (celeris#1228: busy polling is opt-in for the drivers too). The helpers are
// Linux-only; the package holds nothing else on other platforms.
package busypolltest
