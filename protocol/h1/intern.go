package h1

import "bytes"

var (
	bGET     = []byte("GET")
	bPOST    = []byte("POST")
	bPUT     = []byte("PUT")
	bDELETE  = []byte("DELETE")
	bPATCH   = []byte("PATCH")
	bHEAD    = []byte("HEAD")
	bOPTIONS = []byte("OPTIONS")
	bHTTP11  = []byte("HTTP/1.1")
	bHTTP10  = []byte("HTTP/1.0")
	bRoot    = []byte("/")
	bCRLF    = []byte("\r\n")
)

var (
	sGET     = "GET"
	sPOST    = "POST"
	sPUT     = "PUT"
	sDELETE  = "DELETE"
	sPATCH   = "PATCH"
	sHEAD    = "HEAD"
	sOPTIONS = "OPTIONS"
	sHTTP11  = "HTTP/1.1"
	sHTTP10  = "HTTP/1.0"
	sRoot    = "/"
)

func internMethod(b []byte) string {
	switch {
	case bytes.Equal(b, bGET):
		return sGET
	case bytes.Equal(b, bPOST):
		return sPOST
	case bytes.Equal(b, bPUT):
		return sPUT
	case bytes.Equal(b, bDELETE):
		return sDELETE
	case bytes.Equal(b, bPATCH):
		return sPATCH
	case bytes.Equal(b, bHEAD):
		return sHEAD
	case bytes.Equal(b, bOPTIONS):
		return sOPTIONS
	default:
		return string(b)
	}
}

func internVersion(b []byte) string {
	switch {
	case bytes.Equal(b, bHTTP11):
		return sHTTP11
	case bytes.Equal(b, bHTTP10):
		return sHTTP10
	default:
		return string(b)
	}
}

func internPath(b []byte) string {
	if bytes.Equal(b, bRoot) {
		return sRoot
	}
	return string(b)
}
