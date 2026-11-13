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
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testOriginID    = "urn:uuid:00000000-0000-0000-0000-000000000001"
	testTargetURI   = "http://example.com/segmented"
	testSegmentDate = "2024-01-01T00:00:00Z"
)

// segmentBuilder creates a single WARC segment record of the given type.
func segmentBuilder(t *testing.T, recordType RecordType, segmentNumber int, data string, extraHeaders ...[2]string) WarcRecord {
	t.Helper()
	rb := NewRecordBuilder(recordType,
		WithSpecViolationPolicy(ErrIgnore),
		WithSyntaxErrorPolicy(ErrIgnore),
		WithAddMissingDigest(false),
		WithFixDigest(false),
		WithDefaultDigestEncoding(Base16))
	rb.AddWarcHeader(WarcDate, testSegmentDate)
	if recordType == Response {
		rb.AddWarcHeader(ContentType, "application/http;msgtype=response")
	} else {
		rb.AddWarcHeader(ContentType, "text/plain")
	}
	rb.AddWarcHeader(WarcTargetURI, testTargetURI)
	rb.AddWarcHeader(WarcSegmentNumber, fmt.Sprintf("%d", segmentNumber))
	if recordType == Continuation {
		rb.AddWarcHeader(WarcSegmentOriginID, "<"+testOriginID+">")
	} else {
		rb.AddWarcHeader(WarcRecordID, "<"+testOriginID+">")
	}
	for _, h := range extraHeaders {
		rb.AddWarcHeader(h[0], h[1])
	}
	_, err := rb.WriteString(data)
	require.NoError(t, err)
	wr, _, err := rb.Build()
	require.NoError(t, err)
	return wr
}

func buildSegments(t *testing.T, recordType RecordType, parts []string, totalLength int64, lastHeaders ...[2]string) (WarcRecord, []WarcRecord) {
	t.Helper()
	first := segmentBuilder(t, recordType, 1, parts[0])
	continuations := make([]WarcRecord, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		var extra [][2]string
		if i == len(parts)-1 {
			extra = append(extra, [2]string{WarcSegmentTotalLength, fmt.Sprintf("%d", totalLength)})
			extra = append(extra, lastHeaders...)
		}
		continuations = append(continuations, segmentBuilder(t, Continuation, i+1, parts[i], extra...))
	}
	return first, continuations
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestMergeSegments_Response_BasicReassembly(t *testing.T) {
	// The HTTP header spans the boundary between segment 1 and segment 2.
	full := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Length: 13\r\n" +
		"\r\n" +
		"Hello, World!"
	parts := []string{full[:25], full[25:60], full[60:]}

	first, continuations := buildSegments(t, Response, parts, int64(len(full)))
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	// Pass the continuations in reverse order to prove out-of-order reassembly.
	shuffled := []WarcRecord{continuations[1], continuations[0]}
	merged, err := first.Merge(shuffled...)
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	// Identity of the first segment is retained.
	assert.Equal(t, Response, merged.Type())
	assert.Equal(t, testOriginID, merged.RecordId())
	assert.Equal(t, testTargetURI, merged.WarcHeader().Get(WarcTargetURI))
	assert.Equal(t, testSegmentDate, merged.WarcHeader().Get(WarcDate))

	// Segmentation headers are gone.
	assert.False(t, merged.WarcHeader().Has(WarcSegmentNumber))
	assert.False(t, merged.WarcHeader().Has(WarcSegmentOriginID))
	assert.False(t, merged.WarcHeader().Has(WarcSegmentTotalLength))

	// Length and digests are re-established.
	cl, err := merged.ContentLength()
	require.NoError(t, err)
	assert.Equal(t, int64(len(full)), cl)
	assert.Equal(t, sha256Hex(full), merged.WarcHeader().Get(WarcBlockDigest))
	assert.NotEmpty(t, merged.WarcHeader().Get(WarcPayloadDigest))

	// Reassembled bytes match the original content.
	r, err := merged.Block().RawBytes()
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, full, string(got))

	// HTTP headers spanning the segment boundary parse correctly.
	respBlock, ok := merged.Block().(HttpResponseBlock)
	require.True(t, ok)
	assert.Equal(t, 200, respBlock.HttpStatusCode())
	assert.Equal(t, "text/plain", respBlock.HttpHeader().Get("Content-Type"))
	assert.Equal(t, "13", respBlock.HttpHeader().Get("Content-Length"))
	payload, err := respBlock.PayloadBytes()
	require.NoError(t, err)
	payloadBytes, err := io.ReadAll(payload)
	require.NoError(t, err)
	assert.Equal(t, "Hello, World!", string(payloadBytes))
	assert.True(t, merged.Block().IsCached())
}

func TestMergeSegments_Resource_GenericBlock(t *testing.T) {
	full := strings.Repeat("abcdefghij", 100)
	parts := []string{full[:300], full[300:700], full[700:]}

	first, continuations := buildSegments(t, Resource, parts, int64(len(full)))
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	merged, err := first.Merge(continuations...)
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	assert.Equal(t, Resource, merged.Type())
	assert.IsType(t, &genericBlock{}, merged.Block())

	r, err := merged.Block().RawBytes()
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, full, string(got))

	cl, _ := merged.ContentLength()
	assert.Equal(t, int64(len(full)), cl)
	assert.Equal(t, sha256Hex(full), merged.WarcHeader().Get(WarcBlockDigest))
	assert.Equal(t, int64(len(full)), merged.Block().Size())

	// Independently readable more than once.
	r2, err := merged.Block().RawBytes()
	require.NoError(t, err)
	got2, err := io.ReadAll(r2)
	require.NoError(t, err)
	assert.Equal(t, full, string(got2))
}

func TestMergeSegments_TruncatedMarkerFromLastSegment(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nbody"
	parts := []string{full[:20], full[20:]}

	first, continuations := buildSegments(t, Response, parts, int64(len(full)),
		[2]string{WarcTruncated, "length"})
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	merged, err := first.Merge(continuations...)
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	assert.Equal(t, "length", merged.WarcHeader().Get(WarcTruncated))

	// Without a truncation marker on the last segment, the field must be absent even if the
	// first segment carried one.
	first2, cont2 := buildSegments(t, Response, parts, int64(len(full)))
	first2.WarcHeader().Set(WarcTruncated, "time")
	defer func() { assert.NoError(t, first2.Close()) }()
	defer func() {
		for _, c := range cont2 {
			assert.NoError(t, c.Close())
		}
	}()
	merged2, err := first2.Merge(cont2...)
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged2.Close()) }()
	assert.False(t, merged2.WarcHeader().Has(WarcTruncated))
}

func TestMergeSegments_InputsNotModifiedAndStayReadable(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nbody"
	parts := []string{full[:20], full[20:]}

	first, continuations := buildSegments(t, Response, parts, int64(len(full)))
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	merged, err := first.Merge(continuations...)
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	// Input headers keep their segmentation fields.
	assert.True(t, first.WarcHeader().Has(WarcSegmentNumber))
	assert.True(t, continuations[0].WarcHeader().Has(WarcSegmentOriginID))
	assert.True(t, continuations[0].WarcHeader().Has(WarcSegmentTotalLength))
	assert.Equal(t, Continuation, continuations[0].Type())

	// Input blocks remain readable after the merge.
	r, err := first.Block().RawBytes()
	require.NoError(t, err)
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, parts[0], string(b))
	r, err = continuations[0].Block().RawBytes()
	require.NoError(t, err)
	b, err = io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, parts[1], string(b))
}

func TestMergeSegments_BuiltRecordsAreCachedAndReassembled(t *testing.T) {
	// Records produced via the record builder hold their blocks in seekable buffers, which is
	// the supported input for merging. Merge invokes Block.Cache() internally (a no-op for
	// already cached blocks) and reassembles out-of-order continuations.
	full := strings.Repeat("0123456789", 50)
	parts := []string{full[:200], full[200:350], full[350:]}

	first, continuations := buildSegments(t, Resource, parts, int64(len(full)))
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()
	for _, rec := range append([]WarcRecord{first}, continuations...) {
		assert.True(t, rec.Block().IsCached())
	}

	merged, err := first.Merge(continuations[1], continuations[0]) // out of order
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	r, err := merged.Block().RawBytes()
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, full, string(got))
	assert.True(t, merged.Block().IsCached())
}

// TestMergeSegments_DrainedStreamFailsCleanly documents the precondition that blocks must still
// hold their content. A block whose single-use stream was consumed during parsing is reported as
// an explicit failure and no temp resource leaks.
func TestMergeSegments_DrainedStreamFailsCleanly(t *testing.T) {
	tmp := t.TempDir()
	full := strings.Repeat("0123456789", 50)
	p1, p2, p3 := full[:200], full[200:350], full[350:]
	origin := "<urn:uuid:aaaaaaaa-0000-0000-0000-000000000001>"
	raw := buildRawSegmentedWarc(origin, "http://example.com/stream", p1, p2, p3, int64(len(full)))

	u := NewUnmarshaler(WithSpecViolationPolicy(ErrIgnore), WithSyntaxErrorPolicy(ErrIgnore))
	var records []WarcRecord
	// A bare bufio.Reader over NopCloser yields non-seekable, single-read blocks whose content
	// the parser drains while validating digests.
	reader := bufio.NewReader(io.NopCloser(bytes.NewReader(raw)))
	for {
		rec, _, _, err := u.Unmarshal(reader)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		records = append(records, rec)
	}
	require.Len(t, records, 3)
	defer func() {
		for _, rec := range records {
			assert.NoError(t, rec.Close())
		}
	}()
	require.False(t, records[0].Block().IsCached())

	before := listTmpFiles(t, tmp)
	merged, err := records[0].Merge(records[2], records[1])
	require.Error(t, err)
	assert.Nil(t, merged)
	assert.Equal(t, before, listTmpFiles(t, tmp))
}

func buildRawSegmentedWarc(origin, targetURI, p1, p2, p3 string, totalLength int64) []byte {
	var b strings.Builder
	writeRec := func(recType string, id string, extra string, content string) {
		b.WriteString("WARC/1.1\r\n")
		b.WriteString("WARC-Type: " + recType + "\r\n")
		b.WriteString("WARC-Date: " + testSegmentDate + "\r\n")
		b.WriteString("WARC-Record-ID: " + id + "\r\n")
		b.WriteString("WARC-Target-URI: " + targetURI + "\r\n")
		b.WriteString("Content-Type: text/plain\r\n")
		b.WriteString(extra)
		b.WriteString(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(content)))
		b.WriteString(content)
		b.WriteString("\r\n\r\n")
	}
	writeRec("response", origin, "WARC-Segment-Number: 1\r\n", p1)
	writeRec("continuation", "<urn:uuid:bbbbbbbb-0000-0000-0000-000000000002>",
		"WARC-Segment-Number: 2\r\nWARC-Segment-Origin-ID: "+origin+"\r\n", p2)
	writeRec("continuation", "<urn:uuid:cccccccc-0000-0000-0000-000000000003>",
		fmt.Sprintf("WARC-Segment-Number: 3\r\nWARC-Segment-Origin-ID: %s\r\nWARC-Segment-Total-Length: %d\r\n", origin, totalLength), p3)
	return []byte(b.String())
}

func TestMergeSegments_MarshalRoundTrip(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n\r\n<html>reassembled</html>"
	parts := []string{full[:30], full[30:]}

	first, continuations := buildSegments(t, Response, parts, int64(len(full)))
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	merged, err := first.Merge(continuations...)
	require.NoError(t, err)

	var out bytes.Buffer
	m := NewMarshaler()
	continuation, n, err := m.Marshal(&out, merged, 0)
	require.NoError(t, err)
	require.Nil(t, continuation)
	require.NoError(t, merged.Close())
	assert.Equal(t, int64(out.Len()), n)

	// Re-read the serialized merged record.
	u := NewUnmarshaler(WithStrictValidation())
	reread, _, validation, err := u.Unmarshal(bufio.NewReader(bytes.NewReader(out.Bytes())))
	require.NoError(t, err, "validation: %v", validation)
	defer func() { assert.NoError(t, reread.Close()) }()

	assert.Equal(t, Response, reread.Type())
	assert.False(t, reread.WarcHeader().Has(WarcSegmentNumber))
	r, err := reread.Block().RawBytes()
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, full, string(got))

	// The digests written by the merged record must validate on re-parse.
	digestValidation, err := reread.ValidateDigest()
	require.NoError(t, err)
	assert.Empty(t, digestValidation)
}

func TestMergeSegments_MemoryThresholdSpillsToDisk(t *testing.T) {
	tmp := t.TempDir()
	full := strings.Repeat("abcdefghijklmnop", 200) // 3200 bytes
	parts := []string{full[:1000], full[1000:2200], full[2200:]}

	rb := NewRecordBuilder(Resource,
		WithSpecViolationPolicy(ErrIgnore), WithSyntaxErrorPolicy(ErrIgnore),
		WithBufferTmpDir(tmp), WithBufferMaxMemBytes(512),
		WithAddMissingDigest(false), WithFixDigest(false))
	rb.AddWarcHeader(WarcDate, testSegmentDate)
	rb.AddWarcHeader(ContentType, "text/plain")
	rb.AddWarcHeader(WarcTargetURI, testTargetURI)
	rb.AddWarcHeader(WarcSegmentNumber, "1")
	rb.AddWarcHeader(WarcRecordID, "<"+testOriginID+">")
	_, err := rb.WriteString(parts[0])
	require.NoError(t, err)
	first, _, err := rb.Build()
	require.NoError(t, err)
	defer func() { assert.NoError(t, first.Close()) }()

	continuations := make([]WarcRecord, 0, 2)
	for i := 1; i < len(parts); i++ {
		var extra [][2]string
		if i == len(parts)-1 {
			extra = append(extra, [2]string{WarcSegmentTotalLength, fmt.Sprintf("%d", len(full))})
		}
		crb := NewRecordBuilder(Continuation,
			WithSpecViolationPolicy(ErrIgnore), WithSyntaxErrorPolicy(ErrIgnore),
			WithBufferTmpDir(tmp), WithBufferMaxMemBytes(512),
			WithAddMissingDigest(false), WithFixDigest(false))
		crb.AddWarcHeader(WarcDate, testSegmentDate)
		crb.AddWarcHeader(ContentType, "text/plain")
		crb.AddWarcHeader(WarcTargetURI, testTargetURI)
		crb.AddWarcHeader(WarcSegmentNumber, fmt.Sprintf("%d", i+1))
		crb.AddWarcHeader(WarcSegmentOriginID, "<"+testOriginID+">")
		for _, h := range extra {
			crb.AddWarcHeader(h[0], h[1])
		}
		_, err := crb.WriteString(parts[i])
		require.NoError(t, err)
		c, _, err := crb.Build()
		require.NoError(t, err)
		continuations = append(continuations, c)
	}
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	// Temp files are created when the 512 byte memory threshold is exceeded.
	entriesBefore, err := os.ReadDir(tmp)
	require.NoError(t, err)
	require.NotEmpty(t, entriesBefore)

	merged, err := first.Merge(continuations...)
	require.NoError(t, err)

	r, err := merged.Block().RawBytes()
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, full, string(got))

	// Closing the merged record removes its spill file.
	require.NoError(t, merged.Close())
	assert.NoError(t, merged.Close()) // closing twice is safe
}

func TestMergeSegments_FailureCleansUpAndKeepsInputs(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\n\r\nbody"
	parts := []string{full[:10], full[10:]}

	tests := []struct {
		name        string
		mutate      func(first WarcRecord, continuations []WarcRecord)
		wantErr     error
		wantErrText string
	}{
		{
			name: "no continuations",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
			},
			wantErr:     ErrMergeSegmentNumber,
			wantErrText: "segmented merge requires at least one continuation",
		},
		{
			name: "not a continuation record",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].(*warcRecord).recordType = Response
				continuations[0].WarcHeader().Set(WarcType, Response.String())
			},
			wantErr: ErrMergeSegmentRequiresContinuation,
		},
		{
			name: "missing origin id header",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].WarcHeader().Delete(WarcSegmentOriginID)
			},
			wantErr: ErrMergeSegmentMissingHeader,
		},
		{
			name: "origin id mismatch",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].WarcHeader().Set(WarcSegmentOriginID, "<urn:uuid:ffff-0000-0000-0000-000000000099>")
			},
			wantErr: ErrMergeSegmentOriginMismatch,
		},
		{
			name: "missing target uri on continuation",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].WarcHeader().Delete(WarcTargetURI)
			},
			wantErr: ErrMergeSegmentTargetURIMismatch,
		},
		{
			name: "target uri mismatch on continuation",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].WarcHeader().Set(WarcTargetURI, "http://evil.example.com/other")
			},
			wantErr: ErrMergeSegmentTargetURIMismatch,
		},
		{
			name: "missing target uri on first segment",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				first.WarcHeader().Delete(WarcTargetURI)
			},
			wantErr: ErrMergeSegmentTargetURIMismatch,
		},
		{
			name: "duplicate segment number",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].WarcHeader().Set(WarcSegmentNumber, "3")
			},
			wantErr: ErrMergeSegmentNumber,
		},
		{
			name: "gap in segment numbers",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].WarcHeader().Set(WarcSegmentNumber, "4")
			},
			wantErr: ErrMergeSegmentNumber,
		},
		{
			name: "non numeric segment number",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].WarcHeader().Set(WarcSegmentNumber, "two")
			},
			wantErr: ErrMergeSegmentNumber,
		},
		{
			name: "missing segment number header",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].WarcHeader().Delete(WarcSegmentNumber)
			},
			wantErr: ErrMergeSegmentMissingHeader,
		},
		{
			name: "first segment missing segment number",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				first.WarcHeader().Delete(WarcSegmentNumber)
			},
			wantErr: ErrMergeSegmentMissingHeader,
		},
		{
			name: "missing total length header",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].WarcHeader().Delete(WarcSegmentTotalLength)
			},
			wantErr: ErrMergeSegmentTotalLength,
		},
		{
			name: "wrong total length",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				continuations[0].WarcHeader().Set(WarcSegmentTotalLength, "9999")
			},
			wantErr: ErrMergeSegmentTotalLength,
		},
		{
			name: "unsupported first segment type",
			mutate: func(first WarcRecord, continuations []WarcRecord) {
				first.(*warcRecord).recordType = Request
				first.WarcHeader().Set(WarcType, Request.String())
			},
			wantErr: ErrMergeSegmentUnsupportedType,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			first, continuations := buildSegmentsWithTmpDir(t, tmp, Response, parts, int64(len(full)))

			if tt.name != "no continuations" {
				tt.mutate(first, continuations)
			}
			var refs []WarcRecord
			if tt.name != "no continuations" {
				refs = continuations
			}

			before := listTmpFiles(t, tmp)
			got, err := first.Merge(refs...)
			require.Error(t, err)
			assert.Nil(t, got)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			}
			if tt.wantErrText != "" {
				assert.Contains(t, err.Error(), tt.wantErrText)
			}

			// All spill files created during the failed merge must have been removed.
			assert.Equal(t, before, listTmpFiles(t, tmp))

			// Inputs stay open and readable after a failed merge.
			r, rerr := first.Block().RawBytes()
			require.NoError(t, rerr)
			b, rerr := io.ReadAll(r)
			require.NoError(t, rerr)
			assert.Equal(t, parts[0], string(b))

			for _, c := range continuations {
				assert.NoError(t, c.Close())
			}
			assert.NoError(t, first.Close())
		})
	}
}

func buildSegmentsWithTmpDir(t *testing.T, tmp string, recordType RecordType, parts []string, totalLength int64) (WarcRecord, []WarcRecord) {
	t.Helper()
	build := func(rt RecordType, number int, data string, extra ...[2]string) WarcRecord {
		rb := NewRecordBuilder(rt,
			WithSpecViolationPolicy(ErrIgnore), WithSyntaxErrorPolicy(ErrIgnore),
			WithBufferTmpDir(tmp), WithBufferMaxMemBytes(64),
			WithAddMissingDigest(false), WithFixDigest(false))
		rb.AddWarcHeader(WarcDate, testSegmentDate)
		rb.AddWarcHeader(ContentType, "text/plain")
		rb.AddWarcHeader(WarcTargetURI, testTargetURI)
		rb.AddWarcHeader(WarcSegmentNumber, fmt.Sprintf("%d", number))
		if rt == Continuation {
			rb.AddWarcHeader(WarcSegmentOriginID, "<"+testOriginID+">")
		} else {
			rb.AddWarcHeader(WarcRecordID, "<"+testOriginID+">")
		}
		for _, h := range extra {
			rb.AddWarcHeader(h[0], h[1])
		}
		_, err := rb.WriteString(data)
		require.NoError(t, err)
		wr, _, err := rb.Build()
		require.NoError(t, err)
		return wr
	}
	first := build(recordType, 1, parts[0])
	continuations := make([]WarcRecord, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		var extra [][2]string
		if i == len(parts)-1 {
			extra = append(extra, [2]string{WarcSegmentTotalLength, fmt.Sprintf("%d", totalLength)})
		}
		continuations = append(continuations, build(Continuation, i+1, parts[i], extra...))
	}
	return first, continuations
}

func listTmpFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestMergeSegments_MissingTotalLengthFails(t *testing.T) {
	full := "abcdefghij"
	parts := []string{full[:4], full[4:7], full[7:]}

	first, continuations := buildSegments(t, Resource, parts, int64(len(full)))
	continuations[1].WarcHeader().Delete(WarcSegmentTotalLength) // last segment loses the marker
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	got, err := first.Merge(continuations...)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMergeSegmentTotalLength)
}

func TestMergeSegments_NilContinuationFails(t *testing.T) {
	full := "abcdefghij"
	parts := []string{full[:4], full[4:]}

	first, continuations := buildSegments(t, Resource, parts, int64(len(full)))
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	got, err := first.Merge(nil, continuations[0])
	assert.Nil(t, got)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMergeSegmentRequiresContinuation)
}

func TestMergeSegments_MultipleTotalLengthFieldsFail(t *testing.T) {
	full := "abcdefghij"
	parts := []string{full[:4], full[4:7], full[7:]}

	first, continuations := buildSegments(t, Resource, parts, int64(len(full)))
	defer func() { assert.NoError(t, first.Close()) }()
	// Add a total-length field to a non-last segment as well.
	continuations[0].WarcHeader().Set(WarcSegmentTotalLength, fmt.Sprintf("%d", len(full)))
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	got, err := first.Merge(continuations...)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMergeSegmentTotalLength)
}

func TestMergeSegments_TotalLengthOnNonLastSegmentFails(t *testing.T) {
	full := "abcdefghij"
	parts := []string{full[:4], full[4:7], full[7:]}

	first, continuations := buildSegments(t, Resource, parts, int64(len(full)))
	defer func() { assert.NoError(t, first.Close()) }()
	// Move the total-length field from the last to the middle segment.
	continuations[1].WarcHeader().Delete(WarcSegmentTotalLength)
	continuations[0].WarcHeader().Set(WarcSegmentTotalLength, fmt.Sprintf("%d", len(full)))
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	got, err := first.Merge(continuations...)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMergeSegmentTotalLength)
}

func TestMergeSegments_DiskFilesRemovedOnSuccess(t *testing.T) {
	tmp := t.TempDir()
	full := strings.Repeat("xy", 1500)
	parts := []string{full[:1200], full[1200:]}

	first, continuations := buildSegmentsWithTmpDir(t, tmp, Resource, parts, int64(len(full)))
	inputFiles := len(listTmpFiles(t, tmp))
	require.NotZero(t, inputFiles)

	merged, err := first.Merge(continuations...)
	require.NoError(t, err)

	// While everything is open the temp dir holds the input spill files plus the merged
	// record's own spill file (per-segment scratch buffers are released during the merge).
	openFiles := len(listTmpFiles(t, tmp))
	assert.Equal(t, inputFiles+1, openFiles)

	// Closing the merged record removes only its own spill file.
	require.NoError(t, merged.Close())
	assert.Equal(t, inputFiles, len(listTmpFiles(t, tmp)))

	// Closing the inputs removes the rest.
	assert.NoError(t, first.Close())
	for _, c := range continuations {
		assert.NoError(t, c.Close())
	}
	assert.Empty(t, listTmpFiles(t, tmp))
}

func TestMergeSegments_NonHttpContentTypeUsesGenericBlock(t *testing.T) {
	full := "HTTP/1.1 200 OK\r\n\r\nbody" // declared as non-http content type
	parts := []string{full[:12], full[12:]}

	first, continuations := buildSegments(t, Resource, parts, int64(len(full)))
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	merged, err := first.Merge(continuations...)
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()
	assert.IsType(t, &genericBlock{}, merged.Block())
	r, _ := merged.Block().RawBytes()
	got, _ := io.ReadAll(r)
	assert.Equal(t, full, string(got))
}

func TestMergeSegments_PayloadDigestResource(t *testing.T) {
	full := strings.Repeat("resource-payload", 20)
	parts := []string{full[:150], full[150:]}

	first, continuations := buildSegments(t, Resource, parts, int64(len(full)))
	// Simulate a payload digest field on the first segment.
	first.WarcHeader().Set(WarcPayloadDigest, "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() {
		for _, c := range continuations {
			assert.NoError(t, c.Close())
		}
	}()

	merged, err := first.Merge(continuations...)
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	// For a resource the payload is the whole block, so the digest equals the block digest.
	assert.Equal(t, sha256Hex(full), merged.WarcHeader().Get(WarcPayloadDigest))
	assert.Equal(t, sha256Hex(full), merged.WarcHeader().Get(WarcBlockDigest))
}

func TestMergeSegments_TmpDirNotCreatedWhenSmall(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "unused")
	full := "short"
	parts := []string{"sh", "ort"}

	build := func(rt RecordType, number int, data string, extra ...[2]string) WarcRecord {
		rb := NewRecordBuilder(rt,
			WithSpecViolationPolicy(ErrIgnore), WithSyntaxErrorPolicy(ErrIgnore),
			WithBufferTmpDir(tmp), WithBufferMaxMemBytes(1024*1024),
			WithAddMissingDigest(false), WithFixDigest(false))
		rb.AddWarcHeader(WarcDate, testSegmentDate)
		rb.AddWarcHeader(ContentType, "text/plain")
		rb.AddWarcHeader(WarcTargetURI, testTargetURI)
		rb.AddWarcHeader(WarcSegmentNumber, fmt.Sprintf("%d", number))
		if rt == Continuation {
			rb.AddWarcHeader(WarcSegmentOriginID, "<"+testOriginID+">")
		} else {
			rb.AddWarcHeader(WarcRecordID, "<"+testOriginID+">")
		}
		for _, h := range extra {
			rb.AddWarcHeader(h[0], h[1])
		}
		_, err := rb.WriteString(data)
		require.NoError(t, err)
		wr, _, err := rb.Build()
		require.NoError(t, err)
		return wr
	}
	first := build(Resource, 1, parts[0])
	cont := build(Continuation, 2, parts[1], [2]string{WarcSegmentTotalLength, fmt.Sprintf("%d", len(full))})
	defer func() { assert.NoError(t, first.Close()) }()
	defer func() { assert.NoError(t, cont.Close()) }()

	merged, err := first.Merge(cont)
	require.NoError(t, err)
	defer func() { assert.NoError(t, merged.Close()) }()

	_, err = os.Stat(tmp)
	assert.True(t, os.IsNotExist(err))
	r, _ := merged.Block().RawBytes()
	got, _ := io.ReadAll(r)
	assert.Equal(t, full, string(got))
}
