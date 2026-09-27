//line middleware/websocket/compress.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

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

func getFlateWriter(w io.Writer, level int) *flate.Writer {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[0], 1);
	idx := level - minCompressionLevel
	if idx < 0 || idx >= len(flateWriterPools) {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[3], 1);
		idx = defaultCompressionLevel - minCompressionLevel
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[1], 1);if v := flateWriterPools[idx].Get(); v != nil {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[4], 1);
		fw := v.(*flate.Writer)
		fw.Reset(w)
		return fw
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[2], 1);fw, _ := flate.NewWriter(w, level)
	return fw
}

func putFlateWriter(fw *flate.Writer, level int) {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[5], 1);
	idx := level - minCompressionLevel
	if idx < 0 || idx >= len(flateWriterPools) {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[7], 1);
		return
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[6], 1);flateWriterPools[idx].Put(fw)
}

// --- Reader Pool ---

var flateReaderPool sync.Pool

func getFlateReader(r io.Reader) io.ReadCloser {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[8], 1);
	if v := flateReaderPool.Get(); v != nil {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[10], 1);
		fr := v.(io.ReadCloser)
		_ = fr.(flate.Resetter).Reset(r, nil)
		return fr
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[9], 1);return flate.NewReader(r)
}

func putFlateReader(fr io.ReadCloser) {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[11], 1);
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
func (w *truncWriter) Write(p []byte) (int, error) {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[12], 1);
	if len(p) == 0 {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[21], 1);
		return 0, nil
	}

	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[13], 1);total := w.n + len(p)
	if total <= 4 {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[22], 1);
		// All data fits in the hold-back window.
		copy(w.buf[w.n:], p)
		w.n = total
		return len(p), nil
	}

	// Flush everything except the last 4 bytes of the combined stream.
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[14], 1);flushLen := total - 4

	// How many of those flushed bytes come from w.buf vs from p.
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[15], 1);flushFromBuf := w.n
	if flushFromBuf > flushLen {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[23], 1);
		flushFromBuf = flushLen
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[16], 1);if flushFromBuf > 0 {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[24], 1);
		if _, err := w.dst.Write(w.buf[:flushFromBuf]); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[25], 1);
			return 0, err
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[17], 1);flushFromP := flushLen - flushFromBuf
	if flushFromP > 0 {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[26], 1);
		if _, err := w.dst.Write(p[:flushFromP]); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[27], 1);
			return 0, err
		}
	}

	// Reassemble the held-back window from the tail of w.buf (anything
	// not flushed) followed by the tail of p (anything not flushed).
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[18], 1);heldFromBuf := w.n - flushFromBuf
	if heldFromBuf > 0 {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[28], 1);
		copy(w.buf[:heldFromBuf], w.buf[flushFromBuf:w.n])
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[19], 1);heldFromP := len(p) - flushFromP
	if heldFromP > 0 {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[29], 1);
		copy(w.buf[heldFromBuf:heldFromBuf+heldFromP], p[flushFromP:])
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[20], 1);w.n = heldFromBuf + heldFromP
	return len(p), nil
}

// --- Compress / Decompress scratch buffer pool ---

// compressBuf is a pooled io.Writer scratch buffer used by both
// compressMessage and the WriteMessage hot path.
type compressBuf struct {
	data []byte
}

func (b *compressBuf) Write(p []byte) (int, error) {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[30], 1);
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *compressBuf) Reset() {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[31], 1);
	b.data = b.data[:0]
}

const maxPooledScratch = 64 * 1024 // 64 KiB

var compressBufPool = sync.Pool{New: func() any {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[32], 1);
	return &compressBuf{data: make([]byte, 0, 1024)}
}}

func acquireCompressBuf() *compressBuf {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[33], 1);
	return compressBufPool.Get().(*compressBuf)
}

func releaseCompressBuf(b *compressBuf) {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[34], 1);
	if b == nil || cap(b.data) > maxPooledScratch {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[36], 1);
		return
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[35], 1);b.Reset()
	compressBufPool.Put(b)
}

// --- Compress/Decompress Functions ---

// compressMessage compresses data using deflate and writes to dst.
// The trailing sync marker is stripped per RFC 7692.
func compressMessage(dst io.Writer, data []byte, level int) error {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[37], 1);
	tw := &truncWriter{dst: dst}
	fw := getFlateWriter(tw, level)
	if _, err := fw.Write(data); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[40], 1);
		putFlateWriter(fw, level)
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[38], 1);if err := fw.Flush(); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[41], 1);
		putFlateWriter(fw, level)
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[39], 1);putFlateWriter(fw, level)
	return nil
}

// decompressMessage decompresses a permessage-deflate payload. Builds a
// single contiguous source slice (data || deflateTrailer || finalBlock)
// in a pooled scratch buffer, then drains the flate reader into a second
// pooled scratch buffer. Output is bounded by maxOutput so a tiny
// compressed frame cannot expand into an OOM-class payload (decompression
// bomb defense). The returned slice is owned by the caller (a fresh copy)
// so the pool buffers can be reused immediately.
func decompressMessage(data []byte, maxOutput int64) ([]byte, error) {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[42], 1);
	// Build the contiguous source.
	in := acquireCompressBuf()
	defer releaseCompressBuf(in)
	in.data = append(in.data[:0], data...)
	in.data = append(in.data, deflateTrailer...)
	in.data = append(in.data, finalBlock...)

	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[43], 1);src := bytes.NewReader(in.data)
	fr := getFlateReader(src)

	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[44], 1);out := acquireCompressBuf()
	defer releaseCompressBuf(out)
	// LimitReader+1 lets us detect "exactly maxOutput" vs "more than
	// maxOutput" — if Copy reads the +1 byte, the payload exceeds the cap.
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[45], 1);limited := io.LimitReader(fr, maxOutput+1)
	n, err := io.Copy(out, limited)
	putFlateReader(fr)
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[48], 1);
		return nil, err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[46], 1);if n > maxOutput {_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[49], 1);
		return nil, ErrReadLimit
	}

	// Hand back an owned copy of the decompressed bytes.
	_cover_atomic_.AddUint32(&GoCover_c633_compress.Count[47], 1);result := make([]byte, len(out.data))
	copy(result, out.data)
	return result, nil
}

var GoCover_c633_compress = struct {
	Count     [50]uint32
	Pos       [3 * 50]uint32
	NumStmt   [50]uint16
} {
	Pos: [3 * 50]uint32{
		36, 37, 0x2d0002, // [0]
		40, 40, 0x300002, // [1]
		45, 46, 0xb0002, // [2]
		38, 39, 0x10003, // [3]
		41, 44, 0x10003, // [4]
		50, 51, 0x2d0002, // [5]
		54, 54, 0x1f0002, // [6]
		52, 53, 0x10003, // [7]
		62, 62, 0x2a0002, // [8]
		67, 67, 0x1b0002, // [9]
		63, 66, 0x10003, // [10]
		71, 72, 0x10002, // [11]
		89, 89, 0x110002, // [12]
		93, 94, 0x100002, // [13]
		102, 103, 0x10002, // [14]
		105, 106, 0x1d0002, // [15]
		109, 109, 0x160002, // [16]
		114, 115, 0x140002, // [17]
		123, 124, 0x150002, // [18]
		127, 128, 0x130002, // [19]
		131, 132, 0x140002, // [20]
		90, 91, 0x10003, // [21]
		96, 99, 0x10003, // [22]
		107, 108, 0x10003, // [23]
		110, 110, 0x3e0003, // [24]
		111, 112, 0x10004, // [25]
		116, 116, 0x380003, // [26]
		117, 118, 0x10004, // [27]
		125, 126, 0x10003, // [28]
		129, 130, 0x10003, // [29]
		144, 146, 0x10002, // [30]
		149, 150, 0x10002, // [31]
		155, 156, 0x10002, // [32]
		159, 160, 0x10002, // [33]
		163, 163, 0x300002, // [34]
		166, 167, 0x180002, // [35]
		164, 165, 0x10003, // [36]
		175, 177, 0x2a0002, // [37]
		181, 181, 0x230002, // [38]
		185, 186, 0xc0002, // [39]
		178, 180, 0x10003, // [40]
		182, 184, 0x10003, // [41]
		198, 203, 0x10002, // [42]
		204, 206, 0x10002, // [43]
		207, 209, 0x10002, // [44]
		211, 214, 0x100002, // [45]
		217, 217, 0x130002, // [46]
		222, 224, 0x140002, // [47]
		215, 216, 0x10003, // [48]
		218, 219, 0x10003, // [49]
	},
	NumStmt: [50]uint16{
		2, // 0
		1, // 1
		2, // 2
		1, // 3
		3, // 4
		2, // 5
		1, // 6
		1, // 7
		1, // 8
		1, // 9
		3, // 10
		1, // 11
		1, // 12
		2, // 13
		3, // 14
		3, // 15
		1, // 16
		2, // 17
		2, // 18
		2, // 19
		2, // 20
		1, // 21
		3, // 22
		1, // 23
		1, // 24
		1, // 25
		1, // 26
		1, // 27
		1, // 28
		1, // 29
		2, // 30
		1, // 31
		1, // 32
		1, // 33
		1, // 34
		2, // 35
		1, // 36
		3, // 37
		1, // 38
		2, // 39
		2, // 40
		2, // 41
		13, // 42
		13, // 43
		13, // 44
		13, // 45
		1, // 46
		3, // 47
		1, // 48
		1, // 49
	},
}

var _ = _cover_atomic_.LoadUint32
