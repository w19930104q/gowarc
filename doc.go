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

/*
Package gowarc provides a framework for handling WARC files, enabling their parsing, creation, and validation.

# WARC Overview

The WARC format offers a standard way to structure, manage and store billions of resources collected from the web and elsewhere.
It is used to build applications for harvesting, managing, accessing, mining and exchanging content.

For more details, visit the WARC specification: https://iipc.github.io/warc-specifications/specifications/warc-format/warc-1.1/

# WARC record creation

The [WarcRecordBuilder], initialized via [NewRecordBuilder], is the primary tool for creating WARC records.
By default, the WarcRecordBuilder generates a record id and calculates the 'Content-Length' and 'WARC-Block-Digest'.

Use [WarcFileWriter], initialized with [NewWarcFileWriter], to write WARC files.

# WARC record parsing

To parse single WARC records, use the [Unmarshaler] initialized with [NewUnmarshaler].

To read entire WARC files, employ the [WarcFileReader] initialized through [NewWarcFileReader].

# Validation and repair

The gowarc package supports validation during both the creation and parsing of WARC records.
Control over validation and the handling of validation errors can be achieved by setting the appropriate
options in the [WarcRecordBuilder], [Unmarshaler], or [WarcFileReader].

# Record segmentation and merging

Large records may be split by a writer into a first segment of type 'response' or 'resource'
(WARC-Segment-Number: 1) followed by one or more 'continuation' records linked through
WARC-Segment-Origin-ID. Such a record is reassembled with [WarcRecord.Merge]: pass the first segment
as the receiver and all of its continuation records as arguments. The continuations may be supplied
in any order; they are ordered by WARC-Segment-Number during reassembly.

Merge validates that every segment carries a WARC-Target-URI and that the values are identical, that
segment numbers form a consecutive sequence starting at 1, that exactly one (last) segment carries
WARC-Segment-Total-Length matching the reassembled length, and that every continuation references the
first segment's record id. Missing headers, missing or duplicate segments, or a record of the wrong
type make the merge fail with an explicit error.

The result keeps the first segment's identity, adopts a WARC-Truncated marker from the last segment,
drops the segmentation header fields, and re-establishes Content-Length and the block/payload
digests. HTTP headers that span a segment boundary are parsed on the reassembled block. The input
records are not modified and remain the caller's responsibility to close; the returned record owns
its own buffer, is independently readable, and releases its resources on Close (and on failure).
*/
package gowarc
