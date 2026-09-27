// Package zcwindow holds the io_uring SEND_ZC first-completion ->
// notification window open for the celeris#587 race measurement. The hold
// is compiled in only under -tags=validation (zcwindow.go); production
// builds get the no-op in zcwindow_off.go, whose false Enabled constant
// compiles the engine's call sites away. It is internal, not part of the
// public validation package, so nothing outside this module can call it.
package zcwindow
