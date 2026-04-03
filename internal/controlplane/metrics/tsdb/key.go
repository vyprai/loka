package tsdb

import (
	"encoding/binary"
	"math"
)

// Key prefixes for BadgerDB.
const (
	prefixDataPoint  byte = 0x01 // [0x01][series_id:8][timestamp:8]
	prefixSeries     byte = 0x02 // [0x02][series_id:8]
	prefixInverted   byte = 0x03 // [0x03][label_name_len:2][label_name][label_value_len:2][label_value][series_id:8]
	prefixMetricName byte = 0x04 // [0x04][metric_name]
)

// EncodeDataPointKey encodes a data point key: [0x01][series_id:8][timestamp:8]
func EncodeDataPointKey(seriesID uint64, timestampMs int64) []byte {
	key := make([]byte, 1+8+8)
	key[0] = prefixDataPoint
	binary.BigEndian.PutUint64(key[1:9], seriesID)
	binary.BigEndian.PutUint64(key[9:17], uint64(timestampMs))
	return key
}

// DecodeDataPointKey decodes a data point key into series_id and timestamp.
func DecodeDataPointKey(key []byte) (seriesID uint64, timestampMs int64, ok bool) {
	if len(key) != 17 || key[0] != prefixDataPoint {
		return 0, 0, false
	}
	seriesID = binary.BigEndian.Uint64(key[1:9])
	timestampMs = int64(binary.BigEndian.Uint64(key[9:17]))
	return seriesID, timestampMs, true
}

// DataPointKeyPrefix returns the prefix for scanning all data points for a given series.
func DataPointKeyPrefix(seriesID uint64) []byte {
	prefix := make([]byte, 1+8)
	prefix[0] = prefixDataPoint
	binary.BigEndian.PutUint64(prefix[1:9], seriesID)
	return prefix
}

// DataPointKeyRangeStart returns the key to seek to for a time range scan.
func DataPointKeyRangeStart(seriesID uint64, startMs int64) []byte {
	return EncodeDataPointKey(seriesID, startMs)
}

// EncodeSeriesKey encodes a series index key: [0x02][series_id:8]
func EncodeSeriesKey(seriesID uint64) []byte {
	key := make([]byte, 1+8)
	key[0] = prefixSeries
	binary.BigEndian.PutUint64(key[1:9], seriesID)
	return key
}

// DecodeSeriesKey decodes a series index key.
func DecodeSeriesKey(key []byte) (seriesID uint64, ok bool) {
	if len(key) != 9 || key[0] != prefixSeries {
		return 0, false
	}
	return binary.BigEndian.Uint64(key[1:9]), true
}

// EncodeInvertedKey encodes an inverted label index key:
// [0x03][label_name_len:2][label_name][label_value_len:2][label_value][series_id:8]
func EncodeInvertedKey(labelName, labelValue string, seriesID uint64) []byte {
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
	binary.BigEndian.PutUint64(key[off:off+8], seriesID)
	return key
}

// DecodeInvertedKey decodes an inverted label index key.
func DecodeInvertedKey(key []byte) (labelName, labelValue string, seriesID uint64, ok bool) {
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
	seriesID = binary.BigEndian.Uint64(key[off : off+8])
	return labelName, labelValue, seriesID, true
}

// InvertedKeyPrefixLabel returns the prefix for scanning all values of a label name.
// [0x03][label_name_len:2][label_name]
func InvertedKeyPrefixLabel(labelName string) []byte {
	nameBytes := []byte(labelName)
	prefix := make([]byte, 1+2+len(nameBytes))
	prefix[0] = prefixInverted
	binary.BigEndian.PutUint16(prefix[1:3], uint16(len(nameBytes)))
	copy(prefix[3:], nameBytes)
	return prefix
}

// InvertedKeyPrefixLabelValue returns the prefix for scanning all series with a specific label=value.
// [0x03][label_name_len:2][label_name][label_value_len:2][label_value]
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

// EncodeMetricNameKey encodes a metric name index key: [0x04][metric_name]
func EncodeMetricNameKey(name string) []byte {
	key := make([]byte, 1+len(name))
	key[0] = prefixMetricName
	copy(key[1:], name)
	return key
}

// DecodeMetricNameKey decodes a metric name index key.
func DecodeMetricNameKey(key []byte) (name string, ok bool) {
	if len(key) < 2 || key[0] != prefixMetricName {
		return "", false
	}
	return string(key[1:]), true
}

// MetricNameKeyPrefix returns the prefix for scanning all metric names.
func MetricNameKeyPrefix() []byte {
	return []byte{prefixMetricName}
}

// EncodeFloat64 encodes a float64 value as 8 bytes (big-endian uint64 bit pattern).
func EncodeFloat64(v float64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, math.Float64bits(v))
	return buf
}

// DecodeFloat64 decodes a float64 from 8 bytes.
func DecodeFloat64(buf []byte) float64 {
	return math.Float64frombits(binary.BigEndian.Uint64(buf))
}
