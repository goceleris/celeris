//go:build windows

package postgres

// Windows drops the data a client has not read yet when a RST arrives, so a reply
// that was sent in full and followed at once by a reset is a cut there, not a
// complete result: the cells that expect a complete result after an immediate RST
// are not run on Windows (the FIN cells are, and so are the cells that expect an
// error after a cut).
const rstAfterCompleteIsClean = false
