package websocket

import (
	"bytes"
	"compress/flate"
	"io"
	"sync"
)

// Compression levels matching compress/flate.
const (
	CompressionLevelDefault   = flate.DefaultCompression // -1
	CompressionLevelBestSpeed = flate.BestSpeed          // 1
	CompressionLevelBestSize  = flate.BestCompression    // 9
	CompressionLevelHuffman   = flate.HuffmanOnly        // -2
	defaultCompressionLevel   = CompressionLevelBestSpeed
	minCompressionLevel       = flate.HuffmanOnly     // -2
	maxCompressionLevel       = flate.BestCompression // 9
)

// deflateTrailer is the DEFLATE sync flush marker (RFC 7692 Section 7.2.1).
// This trailer is stripped from compressed messages before sending and appended
// before decompressing.
var deflateTrailer = []byte{0x00, 0x00, 0xff, 0xff}

// finalBlock is appended after the trailer to prevent io.ErrUnexpectedEOF
// from the flate reader.
var finalBlock = []byte{0x01, 0x00, 0x00, 0xff, 0xff}

// --- Writer Pool ---

// flateWriterPools has one pool per compression level.
var flateWriterPools [maxCompressionLevel - minCompressionLevel + 1]sync.Pool

func getFlateWriter(w io.Writer, level int) *flate.Writer {
	idx := level - minCompressionLevel
	if idx < 0 || idx >= len(flateWriterPools) {
		idx = defaultCompressionLevel - minCompressionLevel
	}
	if v := flateWriterPools[idx].Get(); v != nil {
		fw := v.(*flate.Writer)
		fw.Reset(w)
		return fw
	}
	fw, _ := flate.NewWriter(w, level)
	return fw
}

func putFlateWriter(fw *flate.Writer, level int) {
	idx := level - minCompressionLevel
	if idx < 0 || idx >= len(flateWriterPools) {
		return
	}
	flateWriterPools[idx].Put(fw)
}

// --- Reader Pool ---

var flateReaderPool sync.Pool

func getFlateReader(r io.Reader) io.ReadCloser {
	if v := flateReaderPool.Get(); v != nil {
		fr := v.(io.ReadCloser)
		_ = fr.(flate.Resetter).Reset(r, nil)
		return fr
	}
	return flate.NewReader(r)
}

func putFlateReader(fr io.ReadCloser) {
	flateReaderPool.Put(fr)
}

// --- truncWriter strips the last 4 bytes (deflate trailer) ---

// truncWriter wraps a writer and holds back the last 4 bytes.
// When Close is called, the held bytes (the deflate sync marker) are discarded.
type truncWriter struct {
	dst io.Writer
	buf [4]byte
	n   int // number of bytes in buf
}

// Write flushes data so that the trailing 4 bytes of the combined stream
// (previously buffered + p) always remain held back in w.buf. The
// held-back bytes may straddle the boundary: when len(p) < 4, some of
// them come from w.buf[:w.n] and the rest from p.
func (w *truncWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	total := w.n + len(p)
	if total <= 4 {
		// All data fits in the hold-back window.
		copy(w.buf[w.n:], p)
		w.n = total
		return len(p), nil
	}

	// Flush everything except the last 4 bytes of the combined stream.
	flushLen := total - 4

	// How many of those flushed bytes come from w.buf vs from p.
	flushFromBuf := w.n
	if flushFromBuf > flushLen {
		flushFromBuf = flushLen
	}
	if flushFromBuf > 0 {
		if _, err := w.dst.Write(w.buf[:flushFromBuf]); err != nil {
			return 0, err
		}
	}
	flushFromP := flushLen - flushFromBuf
	if flushFromP > 0 {
		if _, err := w.dst.Write(p[:flushFromP]); err != nil {
			return 0, err
		}
	}

	// Reassemble the held-back window from the tail of w.buf (anything
	// not flushed) followed by the tail of p (anything not flushed).
	heldFromBuf := w.n - flushFromBuf
	if heldFromBuf > 0 {
		copy(w.buf[:heldFromBuf], w.buf[flushFromBuf:w.n])
	}
	heldFromP := len(p) - flushFromP
	if heldFromP > 0 {
		copy(w.buf[heldFromBuf:heldFromBuf+heldFromP], p[flushFromP:])
	}
	w.n = heldFromBuf + heldFromP
	return len(p), nil
}

// --- Compress / Decompress scratch buffer pool ---

// compressBuf is a pooled io.Writer scratch buffer used by both
// compressMessage and the WriteMessage hot path.
type compressBuf struct {
	data []byte
}

func (b *compressBuf) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *compressBuf) Reset() {
	b.data = b.data[:0]
}

const maxPooledScratch = 64 * 1024 // 64 KiB

var compressBufPool = sync.Pool{New: func() any {
	return &compressBuf{data: make([]byte, 0, 1024)}
}}

func acquireCompressBuf() *compressBuf {
	return compressBufPool.Get().(*compressBuf)
}

func releaseCompressBuf(b *compressBuf) {
	if b == nil || cap(b.data) > maxPooledScratch {
		return
	}
	b.Reset()
	compressBufPool.Put(b)
}

// --- Compress/Decompress Functions ---

// compressMessage compresses data using deflate and writes to dst.
// The trailing sync marker is stripped per RFC 7692.
func compressMessage(dst io.Writer, data []byte, level int) error {
	tw := &truncWriter{dst: dst}
	fw := getFlateWriter(tw, level)
	if _, err := fw.Write(data); err != nil {
		putFlateWriter(fw, level)
		return err
	}
	if err := fw.Flush(); err != nil {
		putFlateWriter(fw, level)
		return err
	}
	putFlateWriter(fw, level)
	return nil
}

// decompressMessage decompresses a permessage-deflate payload. Builds a
// single contiguous source slice (data || deflateTrailer || finalBlock)
// in a pooled scratch buffer, then drains the flate reader into a second
// pooled scratch buffer. Output is bounded by maxOutput so a tiny
// compressed frame cannot expand into an OOM-class payload (decompression
// bomb defense). The returned slice is owned by the caller (a fresh copy)
// so the pool buffers can be reused immediately.
func decompressMessage(data []byte, maxOutput int64) ([]byte, error) {
	// Build the contiguous source.
	in := acquireCompressBuf()
	defer releaseCompressBuf(in)
	in.data = append(in.data[:0], data...)
	in.data = append(in.data, deflateTrailer...)
	in.data = append(in.data, finalBlock...)

	src := bytes.NewReader(in.data)
	fr := getFlateReader(src)

	out := acquireCompressBuf()
	defer releaseCompressBuf(out)
	// LimitReader+1 lets us detect "exactly maxOutput" vs "more than
	// maxOutput" — if Copy reads the +1 byte, the payload exceeds the cap.
	limited := io.LimitReader(fr, maxOutput+1)
	n, err := io.Copy(out, limited)
	putFlateReader(fr)
	if err != nil {
		return nil, err
	}
	if n > maxOutput {
		return nil, ErrReadLimit
	}

	// Hand back an owned copy of the decompressed bytes.
	result := make([]byte, len(out.data))
	copy(result, out.data)
	return result, nil
}
