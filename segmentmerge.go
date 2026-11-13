/*
 * Copyright 2021 National Library of Norway.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *       http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package gowarc

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nlnwa/gowarc/v3/internal/diskbuffer"
)

// mergeSegments reassembles a WARC record that was split into segments.
//
// first is the first segment (WARC-Segment-Number: 1) of type response or resource and continuations
// are the associated continuation records. The continuations may be supplied in any order; they are
// ordered by WARC-Segment-Number during reassembly.
//
// The first segment's identity (record id, date, target URI, ...) is preserved, the segmentation
// header fields are removed, Content-Length and the digests are recalculated and a WARC-Truncated
// field from the last segment is carried over.
//
// Neither the receiver nor the supplied records are modified and closing them remains the caller's
// responsibility. The returned record owns its own, independently readable buffer and releases it
// when Close is called. On failure every buffer allocated by this function is released.
func mergeSegments(first *warcRecord, continuations []WarcRecord) (WarcRecord, error) {
	// --- Validate first segment ---------------------------------------------------------------

	firstNumber, err := parseSegmentNumber(first)
	if err != nil {
		return nil, err
	}
	if firstNumber != 1 {
		return nil, fmt.Errorf("%w: merge receiver must be segment 1, was %d", ErrMergeSegmentNumber, firstNumber)
	}
	if first.recordType != Response && first.recordType != Resource {
		return nil, fmt.Errorf("%w: got %s", ErrMergeSegmentUnsupportedType, first.recordType)
	}
	if first.block == nil {
		return nil, fmt.Errorf("gowarc: first segment has no content block")
	}

	targetURI := first.headers.Get(WarcTargetURI)
	if targetURI == "" {
		return nil, fmt.Errorf("%w: %s", ErrMergeSegmentTargetURIMismatch, WarcTargetURI)
	}
	originID := first.RecordId()
	if originID == "" {
		return nil, fmt.Errorf("%w: %s", ErrMergeSegmentMissingHeader, WarcRecordID)
	}

	if len(continuations) == 0 {
		return nil, fmt.Errorf("%w: segmented merge requires at least one continuation record", ErrMergeSegmentNumber)
	}

	// --- Validate continuation segments -------------------------------------------------------

	segments := make(map[int]WarcRecord, len(continuations))
	var totalLengthHolder WarcRecord
	for i, c := range continuations {
		if c == nil {
			return nil, fmt.Errorf("%w: continuation %d is nil", ErrMergeSegmentRequiresContinuation, i+1)
		}
		if c.Type() != Continuation {
			return nil, fmt.Errorf("%w: record %s is of type %s", ErrMergeSegmentRequiresContinuation, c.RecordId(), c.Type())
		}
		ch := c.WarcHeader()
		if !ch.Has(WarcSegmentOriginID) {
			return nil, fmt.Errorf("%w: %s on segment %s", ErrMergeSegmentMissingHeader, WarcSegmentOriginID, c.RecordId())
		}
		if ch.GetId(WarcSegmentOriginID) != originID {
			return nil, fmt.Errorf("%w: expected %s, got %s", ErrMergeSegmentOriginMismatch, originID, ch.GetId(WarcSegmentOriginID))
		}
		if !ch.Has(WarcSegmentNumber) {
			return nil, fmt.Errorf("%w: %s on segment %s", ErrMergeSegmentMissingHeader, WarcSegmentNumber, c.RecordId())
		}
		n, err := parseSegmentNumber(c)
		if err != nil {
			return nil, err
		}
		if n <= 1 {
			return nil, fmt.Errorf("%w: continuation segment numbers must be greater than 1, got %d", ErrMergeSegmentNumber, n)
		}
		if _, exists := segments[n]; exists {
			return nil, fmt.Errorf("%w: segment %d occurs more than once", ErrMergeSegmentNumber, n)
		}
		if uri := ch.Get(WarcTargetURI); uri == "" || uri != targetURI {
			return nil, fmt.Errorf("%w: expected %q, got %q", ErrMergeSegmentTargetURIMismatch, targetURI, uri)
		}
		if c.Block() == nil {
			return nil, fmt.Errorf("gowarc: segment %d has no content block", n)
		}
		if ch.Has(WarcSegmentTotalLength) {
			if totalLengthHolder != nil {
				return nil, fmt.Errorf("%w: field occurs on more than one segment", ErrMergeSegmentTotalLength)
			}
			totalLengthHolder = c
		}
		segments[n] = c
	}

	lastNumber := len(segments) + 1
	for n := 2; n <= lastNumber; n++ {
		if _, ok := segments[n]; !ok {
			return nil, fmt.Errorf("%w: segment %d is missing", ErrMergeSegmentNumber, n)
		}
	}

	if totalLengthHolder == nil {
		return nil, fmt.Errorf("%w: no segment carries %s", ErrMergeSegmentTotalLength, WarcSegmentTotalLength)
	}
	holderNumber, _ := parseSegmentNumber(totalLengthHolder)
	if holderNumber != lastNumber {
		return nil, fmt.Errorf("%w: field must be on the last segment (%d), was on segment %d",
			ErrMergeSegmentTotalLength, lastNumber, holderNumber)
	}
	declaredTotalLength, err := totalLengthHolder.WarcHeader().GetInt64(WarcSegmentTotalLength)
	if err != nil || declaredTotalLength < 0 {
		return nil, fmt.Errorf("%w: could not parse %s: %q",
			ErrMergeSegmentTotalLength, WarcSegmentTotalLength, totalLengthHolder.WarcHeader().Get(WarcSegmentTotalLength))
	}

	// --- Buffer all segment blocks ------------------------------------------------------------
	//
	// Every block is first cached and then read exactly once into a disk-backed buffer, which
	// honours the configured in-memory threshold (WithBufferMaxMemBytes). Caching is required
	// for non-cached, single-read blocks whose underlying stream may already have been drained
	// by the parser. It mutates only the input block's internal state, never its headers.

	if err := first.block.Cache(); err != nil {
		return nil, fmt.Errorf("failed to cache first segment block: %w", err)
	}
	for n := 2; n <= lastNumber; n++ {
		if err := segments[n].Block().Cache(); err != nil {
			return nil, fmt.Errorf("failed to cache block of segment %d: %w", n, err)
		}
	}

	var buffers []diskbuffer.Buffer
	success := false
	defer func() {
		if !success {
			for _, b := range buffers {
				_ = b.Close()
			}
		}
	}()

	readBlock := func(rec WarcRecord) (diskbuffer.Buffer, error) {
		r, err := rec.Block().RawBytes()
		if err != nil {
			return nil, err
		}
		buf := diskbuffer.New(first.opts.bufferOptions...)
		buffers = append(buffers, buf)
		if _, err := buf.ReadFrom(r); err != nil {
			return nil, err
		}
		return buf, nil
	}

	firstBuf, err := readBlock(first)
	if err != nil {
		return nil, fmt.Errorf("failed to read first segment block: %w", err)
	}
	partBuffers := make([]diskbuffer.Buffer, lastNumber+1)
	partBuffers[1] = firstBuf
	for n := 2; n <= lastNumber; n++ {
		buf, err := readBlock(segments[n])
		if err != nil {
			return nil, fmt.Errorf("failed to read block of segment %d: %w", n, err)
		}
		partBuffers[n] = buf
	}

	// --- Concatenate the parts in segment order -----------------------------------------------

	mergedBuf := diskbuffer.New(first.opts.bufferOptions...)
	buffers = append(buffers, mergedBuf)

	var actualSize int64
	for n := 1; n <= lastNumber; n++ {
		if _, err := partBuffers[n].Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		written, err := mergedBuf.ReadFrom(partBuffers[n])
		if err != nil {
			return nil, err
		}
		actualSize += written
		// The per-segment buffer is no longer needed.
		_ = partBuffers[n].Close()
		partBuffers[n] = nil
	}

	if actualSize != declaredTotalLength {
		return nil, fmt.Errorf("%w: declared %d, reassembled %d",
			ErrMergeSegmentTotalLength, declaredTotalLength, actualSize)
	}

	// --- Build the merged record headers ------------------------------------------------------

	h := first.headers.clone()
	h.Delete(WarcSegmentNumber)
	h.Delete(WarcSegmentOriginID)
	h.Delete(WarcSegmentTotalLength)

	lastHeader := segments[lastNumber].WarcHeader()
	if lastHeader.Has(WarcTruncated) {
		h.Set(WarcTruncated, lastHeader.Get(WarcTruncated))
	} else {
		h.Delete(WarcTruncated)
	}

	blockDigest, err := newDigestForField(h, WarcBlockDigest, first.opts)
	if err != nil {
		return nil, err
	}
	payloadDigest, err := newDigestForField(h, WarcPayloadDigest, first.opts)
	if err != nil {
		return nil, err
	}

	if _, err := mergedBuf.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	// --- Parse the reassembled block ----------------------------------------------------------
	//
	// The full block is available before parsing, so HTTP headers that span a segment boundary
	// are parsed correctly.

	var block Block
	contentType := strings.ToLower(h.Get(ContentType))
	if actualSize == 0 || !strings.HasPrefix(contentType, ApplicationHttp) {
		block = newGenericBlock(first.opts, mergedBuf, blockDigest)
	} else {
		// Non-fatal block parse findings follow the record's error policy inside newHttpBlock;
		// reassembly itself only fails on fatal parse errors.
		block, _, err = newHttpBlock(first.opts, h, mergedBuf, blockDigest, payloadDigest)
		if err != nil {
			return nil, err
		}
	}

	// Force digest calculation over the complete block and re-establish the digest fields.
	blockDigestString := block.BlockDigest()
	h.Set(WarcBlockDigest, blockDigestString)
	if pb, ok := block.(PayloadBlock); ok {
		h.Set(WarcPayloadDigest, pb.PayloadDigest())
	} else if first.recordType == Resource && h.Has(WarcPayloadDigest) {
		// For a resource without a well-defined separate payload, the payload digest equals
		// the block digest.
		h.Set(WarcPayloadDigest, blockDigestString)
	}

	h.SetInt64(ContentLength, block.Size())

	merged := &warcRecord{
		opts:       first.opts,
		version:    first.version,
		recordType: first.recordType,
		headers:    h,
		block:      block,
		closer: func() error {
			return mergedBuf.Close()
		},
	}
	success = true
	return merged, nil
}

// hasContinuation reports whether any of the supplied records is a WARC continuation record.
func hasContinuation(records []WarcRecord) bool {
	for _, r := range records {
		if r != nil && r.Type() == Continuation {
			return true
		}
	}
	return false
}

// parseSegmentNumber extracts and validates the WARC-Segment-Number field of rec.
func parseSegmentNumber(rec WarcRecord) (int, error) {
	h := rec.WarcHeader()
	if !h.Has(WarcSegmentNumber) {
		return 0, fmt.Errorf("%w: %s", ErrMergeSegmentMissingHeader, WarcSegmentNumber)
	}
	n, err := h.GetInt(WarcSegmentNumber)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: %q", ErrMergeSegmentNumber, h.Get(WarcSegmentNumber))
	}
	return n, nil
}

// newDigestForField creates a digest using the algorithm/encoding of the given digest field when
// present, falling back to the record options' default digest configuration.
func newDigestForField(headers *WarcFields, field string, opts *warcRecordOptions) (*digest, error) {
	digestString := opts.defaultDigestAlgorithm
	if headers.Has(field) {
		digestString = headers.Get(field)
	}
	d, err := newDigest(digestString, opts.defaultDigestEncoding)
	if err != nil {
		return nil, errors.Join(ErrUnsupportedDigestAlgorithm, err)
	}
	return d, nil
}
