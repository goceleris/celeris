package frame

const ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

func ValidateClientPreface(buf []byte) bool {
	return len(buf) >= len(ClientPreface) && string(buf[:len(ClientPreface)]) == ClientPreface
}
