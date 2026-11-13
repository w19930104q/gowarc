[![Test](https://github.com/nlnwa/gowarc/actions/workflows/test.yml/badge.svg)](https://github.com/nlnwa/gowarc/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/release/nlnwa/gowarc.svg)](https://github.com/nlnwa/gowarc/releases/latest)
[![License](https://img.shields.io/github/license/nlnwa/gowarc)](/LICENSE)
[![PkgGoDev](https://pkg.go.dev/badge/github.com/nlnwa/gowarc/v3.svg)](https://pkg.go.dev/github.com/nlnwa/gowarc/v3)

# gowarc

A library for creating, parsing and evaluating WARC-files, written in go.

### What is WARC?

The WARC format offers a standard way to structure, manage and store billions of resources collected from the web and
elsewhere. It is used to build applications for harvesting, managing, accessing, mining and exchanging content.

To learn more about the WARC standard, read the specification
at https://iipc.github.io/warc-specifications/specifications/warc-format/warc-1.1/

## Library documentation

#### Installation

```
$ go get github.com/nlnwa/gowarc/v3
```

#### Create a new WARC record

To get you started, here is a simple example of how to create a new WARC record.

```go
package main

import (
	"fmt"
	"github.com/nlnwa/gowarc/v3"
)

func main() {
	builder := gowarc.NewRecordBuilder(gowarc.Response)
	_, err := builder.WriteString("HTTP/1.1 200 OK\nDate: Tue, 19 Sep 2016 17:18:40 GMT\nServer: Apache/2.0.54 (Ubuntu)\n" +
		"Last-Modified: Mon, 16 Jun 2013 22:28:51 GMT\nETag: \"3e45-67e-2ed02ec0\"\nAccept-Ranges: bytes\n" +
		"Content-Length: 19\nConnection: close\nContent-Type: text/plain\n\nThis is the content")
	if err != nil {
		panic(err)
	}
	builder.AddWarcHeader(gowarc.WarcRecordID, "<urn:uuid:e9a0cecc-0221-11e7-adb1-0242ac120008>")
	builder.AddWarcHeader(gowarc.WarcDate, "2006-01-02T15:04:05Z")
	builder.AddWarcHeader(gowarc.ContentLength, "257")
	builder.AddWarcHeader(gowarc.ContentType, "application/http;msgtype=response")
	builder.AddWarcHeader(gowarc.WarcBlockDigest, "sha1:B285747AD7CC57AA74BCE2E30B453C8D1CB71BA4")

	if wr, v, err := builder.Build(); err == nil {
		fmt.Println(wr, v)
	}
}
```

### Merging segmented records

A large record may be split by a writer into a first segment (`response` or `resource` with
`WARC-Segment-Number: 1`) followed by one or more `continuation` records. Reassemble them with
`WarcRecord.Merge`, passing the first segment as the receiver and all continuation records. The
continuations may be given in any order; they are ordered by `WARC-Segment-Number`.

```go
// first is the segment with WARC-Segment-Number: 1, continuations are the related
// 'continuation' records. Blocks must still hold their content (cached or seekable).
merged, err := first.Merge(continuations...)
if err != nil {
    // Explicit errors for missing headers, missing/duplicate segments, origin or
    // target-URI mismatch, non-unique total length, or a non-continuation record.
    log.Fatal(err)
}
defer merged.Close()
```

The merge validates the segment origin id, consecutive segment numbers, a unique terminal
`WARC-Segment-Total-Length` equal to the reassembled length, and that every segment has the same
`WARC-Target-URI`. The result keeps the first segment's identity, adopts a `WARC-Truncated` marker
from the last segment, removes the segmentation headers, and recalculates `Content-Length` and the
block/payload digests. HTTP headers spanning a segment boundary are parsed on the reassembled
block. Input records are not modified and stay the caller's responsibility to close; the returned
record owns an independent, re-readable buffer that is released on `Close`.

### godoc

For complete documentation and examples consult the godoc online at: https://pkg.go.dev/github.com/nlnwa/gowarc/v3


## Command line tools

[warchaeology](https://github.com/nationallibraryofnorway/warchaeology) is a command line tool based on gowarc.
