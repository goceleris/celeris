package h1

// headerEndMarker is the 4-byte sequence \r\n\r\n that terminates HTTP headers.
// Used by SIMD and generic findHeaderEnd implementations.
const headerEndMarker = uint32(0x0a0d0a0d) // \r\n\r\n in little-endian
