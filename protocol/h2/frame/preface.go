package frame

// ClientPreface is the HTTP/2 client connection preface string.
const ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// ValidateClientPreface checks whether buf begins with a valid HTTP/2 client preface.
func ValidateClientPreface(buf []byte) bool {
	return len(buf) >= len(ClientPreface) && string(buf[:len(ClientPreface)]) == ClientPreface
}
