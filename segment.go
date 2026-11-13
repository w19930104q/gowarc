/*
 * Copyright 2024 National Library of Norway.
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
	"cmp"
	"fmt"
	"io"
	"slices"

	"github.com/nlnwa/gowarc/v3/internal/diskbuffer"
)

// mergeSegments merges a segmented record (the first segment, i.e. WARC-Segment-Number: 1)
// with its continuation records into a single record as described by the record segmentation
// chapter of the WARC 1.1 specification:
// https://iipc.github.io/warc-specifications/specifications/warc-format/warc-1.1/#record-segmentation
//
// The continuation records may be supplied in any order. Before any content is read, all
// segments are validated: every continuation must reference the first segment via
// WARC-Segment-Origin-ID, carry the same WARC-Target-URI, and segment numbers must be
// consecutive starting at 2 with exactly one last segment carrying WARC-Segment-Total-Length
// matching the sum of all segment content lengths.
//
// The segment content blocks are concatenated into a buffer backed by the configured buffer
// options, so large records spill to disk instead of being held in memory. The returned
// record holds its own resources and must be closed independently of the input records.
// On failure the input records' header fields are left unmodified.
func (wr *warcRecord) mergeSegments(records ...WarcRecord) (WarcRecord, error) {
	if wr.recordType != Response && wr.recordType != Resource {
		return nil, fmt.Errorf("%w: record type %q", ErrMergeNotSupported, wr.recordType)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("%w: no continuation records supplied", ErrMergeSegmentMissing)
	}

	originID := wr.RecordId()
	targetURI := wr.headers.Get(WarcTargetURI)

	firstLength, err := wr.headers.GetInt64(ContentLength)
	if err != nil {
		return nil, fmt.Errorf("gowarc: cannot parse %s of first segment: %w", ContentLength, err)
	}
	if wr.headers.Has(WarcSegmentTotalLength) {
		return nil, fmt.Errorf("%w: first segment must not carry %s", ErrMergeSegmentLastSegment, WarcSegmentTotalLength)
	}

	type segment struct {
		number   int64
		record   WarcRecord
		length   int64
		total    int64
		hasTotal bool
	}

	segments := make([]segment, 0, len(records))
	seen := make(map[int64]struct{}, len(records))
	for _, rec := range records {
		if rec.Type() != Continuation {
			return nil, fmt.Errorf("%w: got record type %q", ErrMergeSegmentType, rec.Type())
		}
		h := rec.WarcHeader()
		if id := h.GetId(WarcSegmentOriginID); id != originID {
			return nil, fmt.Errorf("%w: expected %q, got %q", ErrMergeSegmentOriginID, originID, id)
		}
		if uri := h.Get(WarcTargetURI); uri != targetURI {
			return nil, fmt.Errorf("%w: expected %q, got %q", ErrMergeSegmentTargetURI, targetURI, uri)
		}
		num, err := h.GetInt64(WarcSegmentNumber)
		if err != nil || num < 2 {
			return nil, fmt.Errorf("%w: value %q in continuation record", ErrMergeSegmentNumber, h.Get(WarcSegmentNumber))
		}
		if _, dup := seen[num]; dup {
			return nil, fmt.Errorf("%w: duplicate segment number %d", ErrMergeSegmentNumber, num)
		}
		seen[num] = struct{}{}
		length, err := h.GetInt64(ContentLength)
		if err != nil {
			return nil, fmt.Errorf("gowarc: cannot parse %s of segment %d: %w", ContentLength, num, err)
		}
		s := segment{number: num, record: rec, length: length}
		if h.Has(WarcSegmentTotalLength) {
			if s.total, err = h.GetInt64(WarcSegmentTotalLength); err != nil {
				return nil, fmt.Errorf("gowarc: cannot parse %s of segment %d: %w", WarcSegmentTotalLength, num, err)
			}
			s.hasTotal = true
		}
		segments = append(segments, s)
	}

	slices.SortFunc(segments, func(a, b segment) int { return cmp.Compare(a.number, b.number) })

	var total int64
	lastSegments := 0
	for i, s := range segments {
		if want := int64(i + 2); s.number != want {
			return nil, fmt.Errorf("%w: segment number %d", ErrMergeSegmentMissing, want)
		}
		if s.hasTotal {
			lastSegments++
			total = s.total
			if i != len(segments)-1 {
				return nil, fmt.Errorf("%w: %s found on segment %d which is not the last segment",
					ErrMergeSegmentLastSegment, WarcSegmentTotalLength, s.number)
			}
		}
	}
	if lastSegments != 1 {
		return nil, fmt.Errorf("%w: found %d segments with %s", ErrMergeSegmentLastSegment, lastSegments, WarcSegmentTotalLength)
	}

	sum := firstLength
	for _, s := range segments {
		sum += s.length
	}
	if sum != total {
		return nil, fmt.Errorf("%w: expected %d, got %d", ErrMergeSegmentTotalLength, total, sum)
	}

	// All metadata validated. Assemble the content into a buffer backed by the configured
	// buffer options so large records spill to disk instead of being held in memory.
	buf := diskbuffer.New(wr.opts.bufferOptions...)
	complete := false
	defer func() {
		if !complete {
			_ = buf.Close()
		}
	}()

	if err := writeBlockContent(buf, wr.block); err != nil {
		return nil, fmt.Errorf("gowarc: failed reading content of first segment: %w", err)
	}
	for _, s := range segments {
		if err := writeBlockContent(buf, s.record.Block()); err != nil {
			return nil, fmt.Errorf("gowarc: failed reading content of segment %d: %w", s.number, err)
		}
	}
	if buf.Size() != total {
		return nil, fmt.Errorf("%w: expected %d bytes, reassembled content was %d bytes", ErrMergeSegmentTotalLength, total, buf.Size())
	}

	// Reassemble the header fields from the first segment per WARC 1.1. The clone ensures
	// the input record's header fields are never modified.
	h := wr.headers.clone()
	h.Delete(WarcSegmentNumber)
	h.Delete(WarcSegmentOriginID)
	h.Delete(WarcSegmentTotalLength)
	h.Delete(WarcBlockDigest)
	h.Delete(WarcPayloadDigest)
	h.SetInt64(ContentLength, total)

	// The truncation reason of the last segment applies to the whole record.
	lastHeader := segments[len(segments)-1].record.WarcHeader()
	if lastHeader.Has(WarcTruncated) {
		h.Set(WarcTruncated, lastHeader.Get(WarcTruncated))
	} else {
		h.Delete(WarcTruncated)
	}

	merged := &warcRecord{
		opts:       wr.opts,
		version:    wr.version,
		recordType: wr.recordType,
		headers:    h,
		closer:     func() error { return buf.Close() },
	}

	if _, err := merged.parseBlock(buf); err != nil {
		return nil, err
	}

	// Recalculate digests for the reassembled record.
	h.Set(WarcBlockDigest, merged.block.BlockDigest())
	if pb, ok := merged.block.(PayloadBlock); ok {
		h.Set(WarcPayloadDigest, pb.PayloadDigest())
	} else if wr.recordType == Resource {
		h.Set(WarcPayloadDigest, merged.block.BlockDigest())
	}

	complete = true
	return merged, nil
}

// writeBlockContent copies the raw content of a block to w.
func writeBlockContent(w io.Writer, block Block) error {
	r, err := block.RawBytes()
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	return err
}
