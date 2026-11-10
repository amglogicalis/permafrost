package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
)

// gearTable holds 256 deterministic 64-bit integers for FastCDC rolling Gear hash.
var gearTable [256]uint64

func init() {
	for i := 0; i < 256; i++ {
		seed := [2]byte{byte(i), 0xA5}
		sum := sha256.Sum256(seed[:])
		gearTable[i] = binary.LittleEndian.Uint64(sum[:8])
	}
}

// RawChunk represents a single content-defined slice read from an input stream.
type RawChunk struct {
	Offset int64
	Data   []byte
}

// FastCDCChunker splits a byte stream into variable-sized content-defined chunks.
type FastCDCChunker struct {
	MinSize int
	AvgSize int
	MaxSize int
	maskS   uint64
	maskL   uint64
}

// NewFastCDCChunker creates a normalized FastCDC chunker with the specified min/avg/max sizes.
func NewFastCDCChunker(minSize, avgSize, maxSize int) *FastCDCChunker {
	if minSize <= 0 {
		minSize = 32 * 1024 // 32 KB
	}
	if avgSize <= minSize {
		avgSize = 128 * 1024 // 128 KB
	}
	if maxSize <= avgSize {
		maxSize = 512 * 1024 // 512 KB
	}

	bits := ilog2(avgSize)
	if bits < 8 {
		bits = 8
	}
	// Normalized FastCDC masks: stricter before AvgSize, looser after AvgSize
	maskS := ((uint64(1) << (bits + 1)) - 1) << 13
	maskL := ((uint64(1) << (bits - 1)) - 1) << 13

	return &FastCDCChunker{
		MinSize: minSize,
		AvgSize: avgSize,
		MaxSize: maxSize,
		maskS:   maskS,
		maskL:   maskL,
	}
}

func ilog2(v int) uint {
	var r uint
	for v > 1 {
		v >>= 1
		r++
	}
	return r
}

// Split reads all content from r and calls fn for each content-defined chunk.
func (c *FastCDCChunker) Split(r io.Reader, fn func(chunk RawChunk) error) error {
	buf := make([]byte, c.MaxSize)
	var streamOffset int64
	var filled int

	for {
		// Fill buffer up to MaxSize
		for filled < c.MaxSize {
			n, err := r.Read(buf[filled:])
			filled += n
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
		}

		if filled == 0 {
			return nil
		}

		cut := c.findCutPoint(buf[:filled])
		chunkBytes := make([]byte, cut)
		copy(chunkBytes, buf[:cut])

		if err := fn(RawChunk{
			Offset: streamOffset,
			Data:   chunkBytes,
		}); err != nil {
			return err
		}

		streamOffset += int64(cut)
		remaining := filled - cut
		if remaining > 0 {
			copy(buf[:remaining], buf[cut:filled])
		}
		filled = remaining
	}
}

// findCutPoint locates the deterministic FastCDC boundary within buf.
func (c *FastCDCChunker) findCutPoint(buf []byte) int {
	n := len(buf)
	if n <= c.MinSize {
		return n
	}

	center := c.AvgSize
	if center > n {
		center = n
	}

	var fp uint64
	// Warm up rolling window 64 bytes before MinSize if possible
	start := c.MinSize - 64
	if start < 0 {
		start = 0
	}
	for i := start; i < c.MinSize; i++ {
		fp = (fp << 1) + gearTable[buf[i]]
	}

	// Region 1: [MinSize, AvgSize) using stricter maskS
	for i := c.MinSize; i < center; i++ {
		fp = (fp << 1) + gearTable[buf[i]]
		if (fp & c.maskS) == 0 {
			return i + 1
		}
	}

	// Region 2: [AvgSize, n) using looser maskL
	for i := center; i < n; i++ {
		fp = (fp << 1) + gearTable[buf[i]]
		if (fp & c.maskL) == 0 {
			return i + 1
		}
	}

	return n
}
