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
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const segmentTestURI = "http://example.com/segmented"

// buildSegmentedRecords builds a segmented record group from the given content parts.
// The first part becomes a record of type recordType with WARC-Segment-Number 1, the
// remaining parts become continuation records. The last continuation carries
// WARC-Segment-Total-Length and, if truncated is not empty, a WARC-Truncated field.
func buildSegmentedRecords(t *testing.T, recordType RecordType, contentType string, parts []string, truncated string, opts ...WarcRecordOption) []WarcRecord {
	t.Helper()
	require.GreaterOrEqual(t, len(parts), 2)

	var total int64
	for _, p := range parts {
		total += int64(len(p))
	}

	rb := NewRecordBuilder(recordType, opts...)
	rb.AddWarcHeader(WarcTargetURI, segmentTestURI)
	rb.AddWarcHeader(WarcDate, "2024-01-01T00:00:00Z")
	rb.AddWarcHeader(ContentType, contentType)
	rb.AddWarcHeaderInt(WarcSegmentNumber, 1)
	_, err := rb.WriteString(parts[0])
	require.NoError(t, err)
	first, _, err := rb.Build()
	require.NoError(t, err)

	records := []WarcRecord{first}
	for i, part := range parts[1:] {
		num := i + 2
		rb := NewRecordBuilder(Continuation, opts...)
		rb.AddWarcHeader(WarcTargetURI, segmentTestURI)
		rb.AddWarcHeader(WarcDate, "2024-01-01T00:00:00Z")
		rb.AddWarcHeaderInt(WarcSegmentNumber, num)
		rb.AddWarcHeader(WarcSegmentOriginID, "<"+first.RecordId()+">")
		if num == len(parts) {
			rb.AddWarcHeaderInt64(WarcSegmentTotalLength, total)
			if truncated != "" {
				rb.AddWarcHeader(WarcTruncated, truncated)
			}
		}
		_, err := rb.WriteString(part)
		require.NoError(t, err)
		rec, _, err := rb.Build()
		require.NoError(t, err)
		records = append(records, rec)
	}
	return records
}

func closeAll(t *testing.T, records ...WarcRecord) {
	t.Helper()
	for _, r := range records {
		assert.NoError(t, r.Close())
	}
}

// roundtripRecord serializes rec, parses it again and returns the parsed record.
func roundtripRecord(t *testing.T, rec WarcRecord, opts ...WarcRecordOption) (WarcRecord, []error) {
	t.Helper()
	var buf bytes.Buffer
	_, _, err := NewMarshaler().Marshal(&buf, rec, 0)
	require.NoError(t, err)

	parsed, _, validation, err := NewUnmarshaler(opts...).Unmarshal(bufio.NewReader(&buf))
	require.NoError(t, err)
	require.NotNil(t, parsed)
	return parsed, validation
}

func readBlockContent(t *testing.T, rec WarcRecord) string {
	t.Helper()
	r, err := rec.Block().RawBytes()
	require.NoError(t, err)
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}

func Test_warcRecord_MergeSegments_Response(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 19\r\nConnection: close\r\n\r\nThis is the content"
	parts := []string{full[:40], full[40:75], full[75:]}

	recs := buildSegmentedRecords(t, Response, "application/http;msgtype=response", parts, "length")
	defer closeAll(t, recs...)

	merged, err := recs[0].Merge(recs[1], recs[2])
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	assert.Equal(t, Response, merged.Type())
	assert.Equal(t, recs[0].RecordId(), merged.RecordId())
	assert.Equal(t, segmentTestURI, merged.WarcHeader().Get(WarcTargetURI))

	// Segment header fields must be gone, Content-Length reflects the whole record
	h := merged.WarcHeader()
	assert.False(t, h.Has(WarcSegmentNumber))
	assert.False(t, h.Has(WarcSegmentOriginID))
	assert.False(t, h.Has(WarcSegmentTotalLength))
	cl, err := h.GetInt64(ContentLength)
	require.NoError(t, err)
	assert.Equal(t, int64(len(full)), cl)

	// Truncation reason inherited from last segment
	assert.Equal(t, "length", h.Get(WarcTruncated))

	// Digests recalculated
	assert.True(t, h.Has(WarcBlockDigest))
	assert.True(t, h.Has(WarcPayloadDigest))

	// Reassembled content and parsed http headers
	assert.Equal(t, full, readBlockContent(t, merged))
	respBlock, ok := merged.Block().(*httpResponseBlock)
	require.True(t, ok)
	assert.Equal(t, 200, respBlock.HttpStatusCode())
	assert.Equal(t, "text/plain", respBlock.HttpHeader().Get("Content-Type"))
	p, err := respBlock.PayloadBytes()
	require.NoError(t, err)
	pb, err := io.ReadAll(p)
	require.NoError(t, err)
	assert.Equal(t, "This is the content", string(pb))

	// Reserialized and reparsed record must pass digest validation
	parsed, validation := roundtripRecord(t, merged, WithStrictValidation())
	defer func() { assert.NoError(t, parsed.Close()) }()
	assert.Empty(t, validation)
	assert.Equal(t, full, readBlockContent(t, parsed))
	digestValidation, err := parsed.ValidateDigest()
	require.NoError(t, err)
	assert.Empty(t, digestValidation)
}

func Test_warcRecord_MergeSegments_OutOfOrder(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 19\r\n\r\nThis is the content"
	parts := []string{full[:10], full[10:25], full[25:50], full[50:]}

	recs := buildSegmentedRecords(t, Response, "application/http;msgtype=response", parts, "")
	defer closeAll(t, recs...)

	// Supply continuations out of order
	merged, err := recs[0].Merge(recs[3], recs[1], recs[2])
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	assert.Equal(t, full, readBlockContent(t, merged))
	// Last segment had no WARC-Truncated, so the merged record must not have one
	assert.False(t, merged.WarcHeader().Has(WarcTruncated))

	parsed, validation := roundtripRecord(t, merged, WithStrictValidation())
	defer func() { assert.NoError(t, parsed.Close()) }()
	assert.Empty(t, validation)
	assert.Equal(t, full, readBlockContent(t, parsed))
}

func Test_warcRecord_MergeSegments_Resource(t *testing.T) {
	full := strings.Repeat("0123456789abcdef", 128)
	parts := []string{full[:100], full[100:1000], full[1000:]}

	recs := buildSegmentedRecords(t, Resource, "application/octet-stream", parts, "time")
	defer closeAll(t, recs...)

	merged, err := recs[0].Merge(recs[2], recs[1])
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	assert.Equal(t, Resource, merged.Type())
	assert.Equal(t, full, readBlockContent(t, merged))
	assert.Equal(t, "time", merged.WarcHeader().Get(WarcTruncated))
	assert.True(t, merged.WarcHeader().Has(WarcBlockDigest))
	assert.True(t, merged.WarcHeader().Has(WarcPayloadDigest))

	parsed, validation := roundtripRecord(t, merged, WithStrictValidation())
	defer func() { assert.NoError(t, parsed.Close()) }()
	assert.Empty(t, validation)
	assert.Equal(t, full, readBlockContent(t, parsed))
}

func Test_warcRecord_MergeSegments_HTTPHeaderBoundary(t *testing.T) {
	// Split points fall in the middle of the http header section, even mid header line
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nHELLO"
	parts := []string{"HTTP/1.1 200 OK\r\nContent-Ty", "pe: text/plain\r\nContent-Length: 5\r\n", "\r\nHELLO"}

	recs := buildSegmentedRecords(t, Response, "application/http;msgtype=response", parts, "")
	defer closeAll(t, recs...)

	merged, err := recs[0].Merge(recs[1], recs[2])
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	assert.Equal(t, full, readBlockContent(t, merged))
	respBlock, ok := merged.Block().(*httpResponseBlock)
	require.True(t, ok)
	assert.Equal(t, 200, respBlock.HttpStatusCode())
	assert.Equal(t, "text/plain", respBlock.HttpHeader().Get("Content-Type"))
	assert.Equal(t, "5", respBlock.HttpHeader().Get("Content-Length"))
	p, err := respBlock.PayloadBytes()
	require.NoError(t, err)
	pb, err := io.ReadAll(p)
	require.NoError(t, err)
	assert.Equal(t, "HELLO", string(pb))

	// The reparsed record must also split headers and payload at the right boundary
	parsed, validation := roundtripRecord(t, merged, WithStrictValidation())
	defer func() { assert.NoError(t, parsed.Close()) }()
	assert.Empty(t, validation)
	parsedBlock, ok := parsed.Block().(*httpResponseBlock)
	require.True(t, ok)
	assert.Equal(t, "text/plain", parsedBlock.HttpHeader().Get("Content-Type"))
}

func Test_warcRecord_MergeSegments_Errors(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nHELLO"
	parts := []string{full[:20], full[20:45], full[45:60], full[60:]}

	newRefs := func(t *testing.T) []WarcRecord {
		return buildSegmentedRecords(t, Response, "application/http;msgtype=response", parts, "")
	}

	tests := []struct {
		name    string
		mutate  func(recs []WarcRecord) (first WarcRecord, refs []WarcRecord)
		wantErr error
	}{
		{
			"no continuation records",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) { return recs[0], nil },
			ErrMergeSegmentMissing,
		},
		{
			"missing segment",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) { return recs[0], []WarcRecord{recs[1], recs[3]} },
			ErrMergeSegmentMissing,
		},
		{
			"duplicate segment number",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) {
				recs[2].WarcHeader().Set(WarcSegmentNumber, "2")
				return recs[0], recs[1:]
			},
			ErrMergeSegmentNumber,
		},
		{
			"invalid segment number",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) {
				recs[1].WarcHeader().Set(WarcSegmentNumber, "x")
				return recs[0], recs[1:]
			},
			ErrMergeSegmentNumber,
		},
		{
			"origin id mismatch",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) {
				recs[1].WarcHeader().Set(WarcSegmentOriginID, "<urn:uuid:8f7e6d5c-4b3a-2c1d-0e9f-8a7b6c5d4e3f>")
				return recs[0], recs[1:]
			},
			ErrMergeSegmentOriginID,
		},
		{
			"target uri mismatch",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) {
				recs[2].WarcHeader().Set(WarcTargetURI, "http://example.com/other")
				return recs[0], recs[1:]
			},
			ErrMergeSegmentTargetURI,
		},
		{
			"no last segment",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) {
				recs[3].WarcHeader().Delete(WarcSegmentTotalLength)
				return recs[0], recs[1:]
			},
			ErrMergeSegmentLastSegment,
		},
		{
			"multiple last segments",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) {
				recs[1].WarcHeader().SetInt64(WarcSegmentTotalLength, int64(len(full)))
				return recs[0], recs[1:]
			},
			ErrMergeSegmentLastSegment,
		},
		{
			"total length mismatch",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) {
				recs[3].WarcHeader().SetInt64(WarcSegmentTotalLength, int64(len(full))+1)
				return recs[0], recs[1:]
			},
			ErrMergeSegmentTotalLength,
		},
		{
			"non continuation input",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) {
				other := createRecord1(Response, &WarcFields{
					&nameValue{Name: WarcDate, Value: "2024-01-01T00:00:00Z"},
					&nameValue{Name: WarcRecordID, Value: "<urn:uuid:00000000-0000-0000-0000-0000000000ff>"},
					&nameValue{Name: ContentType, Value: "text/plain"},
					&nameValue{Name: ContentLength, Value: "0"},
				}, "")
				return recs[0], append([]WarcRecord{other}, recs[1:]...)
			},
			ErrMergeSegmentType,
		},
		{
			"total length on first segment",
			func(recs []WarcRecord) (WarcRecord, []WarcRecord) {
				recs[0].WarcHeader().SetInt64(WarcSegmentTotalLength, int64(len(full)))
				return recs[0], recs[1:]
			},
			ErrMergeSegmentLastSegment,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs := newRefs(t)
			defer closeAll(t, recs...)
			first, refs := tt.mutate(recs)
			defer func() {
				for _, r := range refs {
					_ = r.Close()
				}
			}()

			merged, err := first.Merge(refs...)
			require.Error(t, err)
			assert.Nil(t, merged)
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func Test_warcRecord_MergeSegments_UnsupportedFirstSegmentType(t *testing.T) {
	recs := buildSegmentedRecords(t, Request, "application/http;msgtype=request",
		[]string{"GET / HTTP/1.1\r\n", "Host: example.com\r\n\r\n"}, "")
	defer closeAll(t, recs...)

	merged, err := recs[0].Merge(recs[1])
	require.Error(t, err)
	assert.Nil(t, merged)
	assert.ErrorIs(t, err, ErrMergeNotSupported)
}

func Test_warcRecord_MergeSegments_FailureLeavesInputMetadataUnchanged(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nHELLO"
	parts := []string{full[:20], full[20:45], full[45:]}

	recs := buildSegmentedRecords(t, Response, "application/http;msgtype=response", parts, "length")
	defer closeAll(t, recs...)

	headersBefore := make([]string, len(recs))
	for i, r := range recs {
		var b bytes.Buffer
		_, err := r.WarcHeader().Write(&b)
		require.NoError(t, err)
		headersBefore[i] = b.String()
	}

	// Provoke a failure: wrong total length
	recs[2].WarcHeader().SetInt64(WarcSegmentTotalLength, int64(len(full))+10)
	// Recompute expected header state after the mutation
	var b bytes.Buffer
	_, err := recs[2].WarcHeader().Write(&b)
	require.NoError(t, err)
	headersBefore[2] = b.String()

	merged, err := recs[0].Merge(recs[1], recs[2])
	require.Error(t, err)
	assert.Nil(t, merged)

	for i, r := range recs {
		var got bytes.Buffer
		_, err := r.WarcHeader().Write(&got)
		require.NoError(t, err)
		assert.Equal(t, headersBefore[i], got.String(), "headers of input record %d modified by failed merge", i)
	}
}

func Test_warcRecord_MergeSegments_WrongDigestDetected(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 19\r\n\r\nThis is the content"
	parts := []string{full[:30], full[30:60], full[60:]}

	recs := buildSegmentedRecords(t, Response, "application/http;msgtype=response", parts, "")
	defer closeAll(t, recs...)

	merged, err := recs[0].Merge(recs[1], recs[2])
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	// Serialize and tamper with one byte of the payload
	var buf bytes.Buffer
	_, _, err = NewMarshaler().Marshal(&buf, merged, 0)
	require.NoError(t, err)
	data := buf.Bytes()
	idx := bytes.Index(data, []byte("This is the content"))
	require.GreaterOrEqual(t, idx, 0)
	data[idx] = 'X'

	// ErrWarn policy collects the digest mismatch in the validation slice
	parsed, _, validation, err := NewUnmarshaler().Unmarshal(bufio.NewReader(bytes.NewReader(data)))
	require.NoError(t, err)
	defer func() { assert.NoError(t, parsed.Close()) }()
	var digestErr *DigestError
	found := false
	for _, v := range validation {
		if errors.As(v, &digestErr) {
			found = true
			break
		}
	}
	assert.True(t, found, "expected a DigestError in validation, got %v", validation)

	// ErrFail policy turns the digest mismatch into a fatal error
	_, _, _, err = NewUnmarshaler(WithSpecViolationPolicy(ErrFail)).Unmarshal(bufio.NewReader(bytes.NewReader(data)))
	require.Error(t, err)
	assert.ErrorAs(t, err, &digestErr)
}

func Test_warcRecord_MergeSegments_DiskBufferCleanup(t *testing.T) {
	dir := t.TempDir()
	opts := []WarcRecordOption{WithBufferTmpDir(dir), WithBufferMaxMemBytes(16)}

	body := strings.Repeat("0123456789abcdef", 256) // 4 KiB payload forces disk spill
	full := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	parts := []string{full[:100], full[100:2000], full[2000:]}

	recs := buildSegmentedRecords(t, Response, "application/http;msgtype=response", parts, "", opts...)
	defer closeAll(t, recs...)

	merged, err := recs[0].Merge(recs[2], recs[1])
	require.NoError(t, err)

	// The merged record is backed by the buffer options of the first segment and must
	// not be forced into memory: with a 16 byte memory limit the content spills to disk.
	assert.True(t, merged.Block().IsCached())
	assert.Equal(t, full, readBlockContent(t, merged))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.NotEmpty(t, entries, "expected spill files in tmp dir while records are open")

	// Closing the merged record and the inputs must remove all spill files
	require.NoError(t, merged.Close())
	closeAll(t, recs...)

	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "spill files must be removed after close")
}

func Test_warcRecord_MergeSegments_ResultIndependentOfInputs(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nHELLO"
	parts := []string{full[:25], full[25:50], full[50:]}

	recs := buildSegmentedRecords(t, Response, "application/http;msgtype=response", parts, "")

	merged, err := recs[0].Merge(recs[1], recs[2])
	require.NoError(t, err)

	// Closing the input records must not affect the merged record
	closeAll(t, recs...)
	assert.Equal(t, full, readBlockContent(t, merged))
	assert.NoError(t, merged.Close())
}
