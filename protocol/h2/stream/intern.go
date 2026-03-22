package stream

// Static header name constants for H2 header interning.
// HPACK always emits lowercase names, so these are all lowercase.
var (
	sPseudoMethod    = ":method"
	sPseudoPath      = ":path"
	sPseudoScheme    = ":scheme"
	sPseudoAuthority = ":authority"
	sPseudoStatus    = ":status"

	sAccept           = "accept"
	sAcceptEncoding   = "accept-encoding"
	sAcceptLanguage   = "accept-language"
	sAuthorization    = "authorization"
	sCacheControl     = "cache-control"
	sConnection       = "connection"
	sContentEncoding  = "content-encoding"
	sContentLength    = "content-length"
	sContentType      = "content-type"
	sCookie           = "cookie"
	sDate             = "date"
	sEtag             = "etag"
	sExpect           = "expect"
	sHost             = "host"
	sIfModifiedSince  = "if-modified-since"
	sIfNoneMatch      = "if-none-match"
	sLocation         = "location"
	sOrigin           = "origin"
	sReferer          = "referer"
	sServer           = "server"
	sSetCookie        = "set-cookie"
	sTransferEncoding = "transfer-encoding"
	sUpgrade          = "upgrade"
	sUserAgent        = "user-agent"
	sVary             = "vary"
	sXForwardedFor    = "x-forwarded-for"
	sXRealIP          = "x-real-ip"
	sXRequestID       = "x-request-id"
)

// internH2HeaderName returns a static string for common HTTP/2 header names,
// allowing the HPACK-allocated string to be GC'd. Uses length+first-byte
// dispatch for O(1) lookup. Returns the input string unchanged for unknown names.
func internH2HeaderName(name string) string {
	switch len(name) {
	case 4:
		switch name[0] {
		case 'd':
			if name == "date" {
				return sDate
			}
		case 'e':
			if name == "etag" {
				return sEtag
			}
		case 'h':
			if name == "host" {
				return sHost
			}
		case 'v':
			if name == "vary" {
				return sVary
			}
		}
	case 5:
		if name == ":path" {
			return sPseudoPath
		}
	case 6:
		switch name[0] {
		case 'a':
			if name == "accept" {
				return sAccept
			}
		case 'c':
			if name == "cookie" {
				return sCookie
			}
		case 'e':
			if name == "expect" {
				return sExpect
			}
		case 'o':
			if name == "origin" {
				return sOrigin
			}
		case 's':
			if name == "server" {
				return sServer
			}
		}
	case 7:
		switch name[0] {
		case ':':
			if name == ":method" {
				return sPseudoMethod
			}
			if name == ":scheme" {
				return sPseudoScheme
			}
			if name == ":status" {
				return sPseudoStatus
			}
		case 'r':
			if name == "referer" {
				return sReferer
			}
		case 'u':
			if name == "upgrade" {
				return sUpgrade
			}
		}
	case 8:
		if name == "location" {
			return sLocation
		}
	case 9:
		if name == "x-real-ip" {
			return sXRealIP
		}
	case 10:
		switch name[0] {
		case ':':
			if name == ":authority" {
				return sPseudoAuthority
			}
		case 'c':
			if name == "connection" {
				return sConnection
			}
		case 's':
			if name == "set-cookie" {
				return sSetCookie
			}
		case 'u':
			if name == "user-agent" {
				return sUserAgent
			}
		}
	case 12:
		switch name[0] {
		case 'c':
			if name == "content-type" {
				return sContentType
			}
		case 'x':
			if name == "x-request-id" {
				return sXRequestID
			}
		}
	case 13:
		switch name[0] {
		case 'a':
			if name == "authorization" {
				return sAuthorization
			}
		case 'c':
			if name == "cache-control" {
				return sCacheControl
			}
		case 'i':
			if name == "if-none-match" {
				return sIfNoneMatch
			}
		}
	case 14:
		if name == "content-length" {
			return sContentLength
		}
	case 15:
		switch name[0] {
		case 'a':
			if name == "accept-encoding" {
				return sAcceptEncoding
			}
			if name == "accept-language" {
				return sAcceptLanguage
			}
		case 'x':
			if name == "x-forwarded-for" {
				return sXForwardedFor
			}
		}
	case 16:
		if name == "content-encoding" {
			return sContentEncoding
		}
	case 17:
		switch name[0] {
		case 'i':
			if name == "if-modified-since" {
				return sIfModifiedSince
			}
		case 't':
			if name == "transfer-encoding" {
				return sTransferEncoding
			}
		}
	}
	return name
}
