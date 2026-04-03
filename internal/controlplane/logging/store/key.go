package store

import (
	"encoding/binary"
)

// Key prefixes for BadgerDB log store.
const (
	prefixLogEntry  byte = 0x10 // [0x10][stream_id:8][timestamp_ns:8]
	prefixStream    byte = 0x11 // [0x11][stream_id:8]
	prefixInverted  byte = 0x12 // [0x12][label_name_len:2][name][label_value_len:2][value][stream_id:8]
	prefixLabelName byte = 0x13 // [0x13][label_name]
)

// EncodeLogEntryKey encodes a log entry key: [0x10][stream_id:8][timestamp_ns:8]
func EncodeLogEntryKey(streamID uint64, timestampNs int64) []byte {
	key := make([]byte, 1+8+8)
	key[0] = prefixLogEntry
	binary.BigEndian.PutUint64(key[1:9], streamID)
	binary.BigEndian.PutUint64(key[9:17], uint64(timestampNs))
	return key
}

// DecodeLogEntryKey decodes a log entry key into stream_id and timestamp (nanoseconds).
func DecodeLogEntryKey(key []byte) (streamID uint64, timestampNs int64, ok bool) {
	if len(key) != 17 || key[0] != prefixLogEntry {
		return 0, 0, false
	}
	streamID = binary.BigEndian.Uint64(key[1:9])
	timestampNs = int64(binary.BigEndian.Uint64(key[9:17]))
	return streamID, timestampNs, true
}

// LogEntryKeyPrefix returns the prefix for scanning all log entries for a given stream.
func LogEntryKeyPrefix(streamID uint64) []byte {
	prefix := make([]byte, 1+8)
	prefix[0] = prefixLogEntry
	binary.BigEndian.PutUint64(prefix[1:9], streamID)
	return prefix
}

// LogEntryKeyRangeStart returns the key to seek to for a time range scan.
func LogEntryKeyRangeStart(streamID uint64, startNs int64) []byte {
	return EncodeLogEntryKey(streamID, startNs)
}

// EncodeStreamKey encodes a stream index key: [0x11][stream_id:8]
func EncodeStreamKey(streamID uint64) []byte {
	key := make([]byte, 1+8)
	key[0] = prefixStream
	binary.BigEndian.PutUint64(key[1:9], streamID)
	return key
}

// DecodeStreamKey decodes a stream index key.
func DecodeStreamKey(key []byte) (streamID uint64, ok bool) {
	if len(key) != 9 || key[0] != prefixStream {
		return 0, false
	}
	return binary.BigEndian.Uint64(key[1:9]), true
}

// EncodeInvertedKey encodes an inverted label index key:
// [0x12][label_name_len:2][label_name][label_value_len:2][label_value][stream_id:8]
func EncodeInvertedKey(labelName, labelValue string, streamID uint64) []byte {
	nameBytes := []byte(labelName)
	valueBytes := []byte(labelValue)
	key := make([]byte, 1+2+len(nameBytes)+2+len(valueBytes)+8)
	off := 0
	key[off] = prefixInverted
	off++
	binary.BigEndian.PutUint16(key[off:off+2], uint16(len(nameBytes)))
	off += 2
	copy(key[off:], nameBytes)
	off += len(nameBytes)
	binary.BigEndian.PutUint16(key[off:off+2], uint16(len(valueBytes)))
	off += 2
	copy(key[off:], valueBytes)
	off += len(valueBytes)
	binary.BigEndian.PutUint64(key[off:off+8], streamID)
	return key
}

// DecodeInvertedKey decodes an inverted label index key.
func DecodeInvertedKey(key []byte) (labelName, labelValue string, streamID uint64, ok bool) {
	if len(key) < 1+2+0+2+0+8 || key[0] != prefixInverted {
		return "", "", 0, false
	}
	off := 1
	nameLen := int(binary.BigEndian.Uint16(key[off : off+2]))
	off += 2
	if off+nameLen > len(key) {
		return "", "", 0, false
	}
	labelName = string(key[off : off+nameLen])
	off += nameLen
	if off+2 > len(key) {
		return "", "", 0, false
	}
	valueLen := int(binary.BigEndian.Uint16(key[off : off+2]))
	off += 2
	if off+valueLen+8 > len(key) {
		return "", "", 0, false
	}
	labelValue = string(key[off : off+valueLen])
	off += valueLen
	streamID = binary.BigEndian.Uint64(key[off : off+8])
	return labelName, labelValue, streamID, true
}

// InvertedKeyPrefixLabel returns the prefix for scanning all values of a label name.
// [0x12][label_name_len:2][label_name]
func InvertedKeyPrefixLabel(labelName string) []byte {
	nameBytes := []byte(labelName)
	prefix := make([]byte, 1+2+len(nameBytes))
	prefix[0] = prefixInverted
	binary.BigEndian.PutUint16(prefix[1:3], uint16(len(nameBytes)))
	copy(prefix[3:], nameBytes)
	return prefix
}

// InvertedKeyPrefixLabelValue returns the prefix for scanning all streams with a specific label=value.
// [0x12][label_name_len:2][label_name][label_value_len:2][label_value]
func InvertedKeyPrefixLabelValue(labelName, labelValue string) []byte {
	nameBytes := []byte(labelName)
	valueBytes := []byte(labelValue)
	prefix := make([]byte, 1+2+len(nameBytes)+2+len(valueBytes))
	off := 0
	prefix[off] = prefixInverted
	off++
	binary.BigEndian.PutUint16(prefix[off:off+2], uint16(len(nameBytes)))
	off += 2
	copy(prefix[off:], nameBytes)
	off += len(nameBytes)
	binary.BigEndian.PutUint16(prefix[off:off+2], uint16(len(valueBytes)))
	off += 2
	copy(prefix[off:], valueBytes)
	return prefix
}

// EncodeLabelNameKey encodes a label catalog key: [0x13][label_name]
func EncodeLabelNameKey(name string) []byte {
	key := make([]byte, 1+len(name))
	key[0] = prefixLabelName
	copy(key[1:], name)
	return key
}

// DecodeLabelNameKey decodes a label catalog key.
func DecodeLabelNameKey(key []byte) (name string, ok bool) {
	if len(key) < 2 || key[0] != prefixLabelName {
		return "", false
	}
	return string(key[1:]), true
}

// LabelNameKeyPrefix returns the prefix for scanning all label names.
func LabelNameKeyPrefix() []byte {
	return []byte{prefixLabelName}
}

// InvertedKeyPrefixAll returns just the inverted index prefix byte for scanning all entries.
func InvertedKeyPrefixAll() []byte {
	return []byte{prefixInverted}
}
