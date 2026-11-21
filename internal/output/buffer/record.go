package buffer

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

// A record on disk is a header followed by its payload, compressed:
//
//	length  uint32  length of the payload on disk
//	crc     uint32  CRC-32C of everything after it (seq to the payload)
//	seq     uint64  sequence number, increasing across segments
//	time    int64   time of the record (Unix nanoseconds), for maxAge
//	count   uint32  points in the record, for the accounting of drops
//	payload [length]byte
//
// Integers are little endian.
const headerSize = 4 + 4 + 8 + 8 + 4

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// errCorrupt reports a record whose checksum or length does not hold.
var errCorrupt = errors.New("corrupt record")

// header is the decoded header of a record.
type header struct {
	length uint32
	crc    uint32
	seq    uint64
	time   int64
	count  uint32
}

// encodeRecord returns the bytes of a record, ready to be written at once.
func encodeRecord(seq uint64, unixNano int64, count uint32, payload []byte) []byte {
	b := make([]byte, headerSize+len(payload))
	binary.LittleEndian.PutUint32(b[0:], uint32(len(payload))) //nolint:gosec // G115: Append keeps payloads under maxRecordLength.
	binary.LittleEndian.PutUint64(b[8:], seq)
	binary.LittleEndian.PutUint64(b[16:], uint64(unixNano)) //nolint:gosec // G115: the bits of the time, read back as int64.
	binary.LittleEndian.PutUint32(b[24:], count)
	copy(b[headerSize:], payload)
	binary.LittleEndian.PutUint32(b[4:], crc32.Checksum(b[8:], castagnoli))
	return b
}

// readRecord reads the record at the start of r. It returns io.EOF at a
// clean end, io.ErrUnexpectedEOF for a record cut short (a write a crash
// interrupted) and errCorrupt for a record whose checksum does not hold.
func readRecord(r io.Reader, maxLength uint32) (header, []byte, error) {
	var hb [headerSize]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return header{}, nil, err
	}
	h := header{
		length: binary.LittleEndian.Uint32(hb[0:]),
		crc:    binary.LittleEndian.Uint32(hb[4:]),
		seq:    binary.LittleEndian.Uint64(hb[8:]),
		time:   int64(binary.LittleEndian.Uint64(hb[16:])), //nolint:gosec // G115: the bits written from an int64.
		count:  binary.LittleEndian.Uint32(hb[24:]),
	}
	if h.length > maxLength {
		return h, nil, errCorrupt
	}
	payload := make([]byte, h.length)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return h, nil, err
	}
	crc := crc32.New(castagnoli)
	_, _ = crc.Write(hb[8:])
	_, _ = crc.Write(payload)
	if crc.Sum32() != h.crc {
		return h, nil, errCorrupt
	}
	return h, payload, nil
}
