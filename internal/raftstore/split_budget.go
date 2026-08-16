package raftstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"

	"github.com/andrew01234567890/vbdb/internal/storage"
)

const splitMapEntryBytes = 96

func splitMemoryAdd(a, b int) (int, error) {
	if a < 0 || b < 0 || a > math.MaxInt-b {
		return 0, ErrBackpressure
	}
	return a + b, nil
}

func splitMemorySum(values ...int) (int, error) {
	total := 0
	for _, value := range values {
		var err error
		total, err = splitMemoryAdd(total, value)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func splitMemoryMul(a, b int) (int, error) {
	if a < 0 || b < 0 || (a != 0 && b > math.MaxInt/a) {
		return 0, ErrBackpressure
	}
	return a * b, nil
}

func splitDescriptorMemory(descriptor RangeDescriptor) int {
	return 64 + len(descriptor.RangeID) + len(descriptor.Start) + len(descriptor.End) + len(descriptor.Voters)*8
}

func splitRowMemory(row storage.Row) (int, error) {
	return splitMemorySum(64, len(row.Table), len(row.Key), 16, len(row.Value))
}

func splitSnapshotMemory(snapshot SplitSnapshot) (int, error) {
	total, err := splitMemorySum(splitDescriptorMemory(snapshot.Source), len(snapshot.Rows)*64, len(snapshot.Rows)*splitMapEntryBytes)
	if err != nil {
		return 0, err
	}
	for _, row := range snapshot.Rows {
		rowBytes, rowErr := splitRowMemory(row)
		if rowErr != nil {
			return 0, rowErr
		}
		total, err = splitMemoryAdd(total, rowBytes)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func splitDeltaMemory(delta SplitDelta) (int, error) {
	command, err := EncodeCommand(delta.Command)
	if err != nil {
		return 0, err
	}
	result, err := encodeResult(delta.Result)
	if err != nil {
		return 0, err
	}
	return splitMemorySum(128, len(delta.SourceRangeID), len(delta.SourceStart), len(delta.SourceEnd), len(delta.SourceVoters)*8, len(command), len(result))
}

func splitTransferScratchMemory(source RangeDescriptor, rows []storage.Row) (int, error) {
	encoded, err := splitSnapshotEncodedSize(source, rows)
	if err != nil || encoded > maxSplitSnapshotBytes {
		return 0, ErrBackpressure
	}
	chunks := (encoded + maxSplitCopyChunkBytes - splitChunkHeaderBytes - 1) / (maxSplitCopyChunkBytes - splitChunkHeaderBytes)
	chunkBytes, err := splitMemoryMul(chunks, maxSplitCopyChunkBytes)
	if err != nil {
		return 0, err
	}
	validation, err := splitRowValidationScratchMemoryCount(len(rows))
	if err != nil {
		return 0, err
	}
	return splitMemorySum(encoded, chunkBytes, validation, splitDescriptorMemory(source))
}

func splitCatchUpScratchMemory(deltas []SplitDelta) (int, error) {
	if len(deltas) > maxSplitDeltas {
		return 0, ErrBackpressure
	}
	total, err := splitMemoryMul(len(deltas), splitMapEntryBytes)
	if err != nil {
		return 0, err
	}
	for _, delta := range deltas {
		memory, memoryErr := splitDeltaMemory(delta)
		if memoryErr != nil {
			return 0, memoryErr
		}
		total, err = splitMemoryAdd(total, memory)
		if err != nil || total > maxSplitRetainedBytes {
			return 0, ErrBackpressure
		}
	}
	return total, nil
}

func splitRowValidationScratchMemoryMap(rows map[string]storage.Row) (int, error) {
	total, err := splitRowValidationScratchMemoryCount(len(rows))
	if err != nil {
		return 0, err
	}
	for key := range rows {
		if len(key) > maxCommandTable+maxCommandKey+8 {
			return 0, ErrSplitChecksum
		}
		total, err = splitMemoryAdd(total, len(key))
		if err != nil || total > maxSplitValidationBytes {
			return 0, ErrBackpressure
		}
	}
	return total, nil
}

func splitRowsDigest(rows map[string]storage.Row) [32]byte {
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var encoded bytes.Buffer
	for _, key := range keys {
		row := rows[key]
		putSplitString(&encoded, key)
		encoded.Write(row.Version[:])
		var number [8]byte
		binary.BigEndian.PutUint64(number[:], row.Sequence)
		encoded.Write(number[:])
		putSplitBytes(&encoded, row.Value)
	}
	return sha256.Sum256(encoded.Bytes())
}
