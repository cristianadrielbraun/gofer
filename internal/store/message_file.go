package store

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// Message files (the raw message and its text and HTML bodies) are saved
// compressed with zstd when that makes them smaller. Readers go through
// ReadMessageFile or OpenMessageFile, which recognise a compressed file by
// its zstd frame header and read older, uncompressed files as they are.
// Attachments and images are already compressed and are saved unchanged.

// maxMessageFileBytes bounds what a compressed message file may expand to:
// twice the largest message Gofer downloads, which leaves room for bodies
// that grow when they are decoded or cleaned.
const maxMessageFileBytes = 128 << 20

// Files smaller than this are saved as they are; the gain is too small to be
// worth decompressing on every read.
const minCompressedMessageFileBytes = 512

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

var (
	messageFileEncoder, _ = zstd.NewWriter(nil,
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithLowerEncoderMem(true))
	messageFileDecoder, _ = zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(0),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(maxMessageFileBytes))
)

// encodeMessageFile returns the bytes to save for data: compressed when that
// is smaller, else data itself.
func encodeMessageFile(data []byte) []byte {
	if len(data) < minCompressedMessageFileBytes {
		return data
	}
	compressed := messageFileEncoder.EncodeAll(data, make([]byte, 0, len(data)/2))
	if len(compressed) >= len(data) {
		return data
	}
	return compressed
}

// writeMessageFile saves data at path, compressed when that is smaller.
func writeMessageFile(path string, data []byte) error {
	return os.WriteFile(path, encodeMessageFile(data), 0644)
}

// decodeMessageFile returns the content of a saved message file.
func decodeMessageFile(stored []byte) ([]byte, error) {
	if !bytes.HasPrefix(stored, zstdMagic) {
		return stored, nil
	}
	data, err := messageFileDecoder.DecodeAll(stored, nil)
	if err != nil {
		return nil, fmt.Errorf("decompress message file: %w", err)
	}
	return data, nil
}

// ReadMessageFile returns the content of a raw message or body file, whether
// it was saved compressed or not.
func ReadMessageFile(path string) ([]byte, error) {
	stored, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeMessageFile(stored)
}

// RewriteMessageFile replaces the content of a saved raw message or body
// file with what change makes of it, compressed the way new files are. When
// change reports false, the file is left as it is. The new content is
// written beside the file and renamed over it, so a reader never sees part
// of it, and a file that changes meanwhile is refused. It reports whether
// the file was rewritten.
func RewriteMessageFile(path string, change func([]byte) ([]byte, bool)) (bool, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !before.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	data, err := decodeMessageFile(stored)
	if err != nil {
		return false, err
	}
	changed, ok := change(data)
	if !ok {
		return false, nil
	}
	if err := replaceIfUnchanged(path, before, encodeMessageFile(changed)); err != nil {
		return false, fmt.Errorf("rewrite %s: %w", path, err)
	}
	return true, nil
}

// OpenMessageFile is ReadMessageFile for callers that want a reader.
func OpenMessageFile(path string) (io.ReadCloser, error) {
	data, err := ReadMessageFile(path)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
