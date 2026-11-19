// makefun anchor d5c686788dbb1f4b8339ab1831fbc48fa30b7a03 round 34097789
//
// gitobj.go — the git object model, pkt-line codec and packfile read/write.
// Port of github-core-clone-01-v3/gitobj.ts (757 lines), behaviour-identical
// where the DESIGN.md signatures allow. Pure functions over []byte: no I/O,
// no store. Stdlib only.
//
// Object formats follow git exactly:
//
//	commit: "tree <oid>\n" + "parent <oid>\n"* + "author <who> <ts> +0000\n"
//	        + "committer <who> <ts> +0000\n" + "\n" + message
//	tag:    "object <oid>\ntype <type>\ntag <name>\ntagger <who> <ts> +0000\n"
//	        + "\n" + message
//	tree:   "<mode> <name>\0<20-byte oid>" per entry, sorted in git's order
//	        (directories compare as name + "/").
//	blob:   the raw bytes.
//
// Packfiles are version 2, plain objects (no deltas on write; deltas are
// resolved on read so a delta pack never breaks an unpack).
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash/adler32"
	"io"
	"sort"
	"strconv"
	"strings"
)

type ObjType string

const (
	ObjBlob   ObjType = "blob"
	ObjTree   ObjType = "tree"
	ObjCommit ObjType = "commit"
	ObjTag    ObjType = "tag"
)

type GitObject struct {
	Type ObjType
	Data []byte
}

type TreeEntry struct {
	Mode, Name, OID string
}

type Identity struct {
	Name, Email string
}

type ParsedCommit struct {
	Tree      string
	Parents   []string
	Author    Identity
	Committer Identity
	Timestamp int64
	Message   string
}

type ParsedTag struct {
	Object    string
	Type      ObjType
	Tag       string
	Tagger    Identity
	Timestamp int64
	Message   string
}

type CommitInput struct {
	Tree      string
	Parents   []string
	Message   string
	Author    Identity
	Committer Identity
	Timestamp int64
}

type TagInput struct {
	Object    string
	Type      ObjType
	Tag       string
	Tagger    Identity
	Timestamp int64
	Message   string
}

var typeCodes = map[ObjType]int{
	ObjCommit: 1,
	ObjTree:   2,
	ObjBlob:   3,
	ObjTag:    4,
}

var codeTypes = map[int]ObjType{
	1: ObjCommit,
	2: ObjTree,
	3: ObjBlob,
	4: ObjTag,
}

// HashObject returns the id of an object: SHA-1 over "<type> <size>\0" + data.
func HashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", string(typ), len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// treeSortKey is git's tree order: directories compare as their name plus "/".
func treeSortKey(entry TreeEntry) string {
	if entry.Mode == "40000" {
		return entry.Name + "/"
	}
	return entry.Name
}

// BuildTree builds a tree object from entries (sorted in git's order).
// The input slice is not mutated.
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var out bytes.Buffer
	for _, entry := range sorted {
		out.WriteString(entry.Mode)
		out.WriteByte(' ')
		out.WriteString(entry.Name)
		out.WriteByte(0)
		raw, _ := hex.DecodeString(entry.OID)
		out.Write(raw)
	}
	return out.Bytes()
}

// ParseTree parses a tree object into its entries. A malformed entry stops
// the parse and the entries decoded so far are returned (the TS reference
// throws; this signature has no error channel).
func ParseTree(data []byte) []TreeEntry {
	entries := []TreeEntry{}
	at := 0
	for at < len(data) {
		space := bytes.IndexByte(data[at:], ' ')
		if space < 0 {
			break
		}
		space += at
		mode := string(data[at:space])
		rest := data[space+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 {
			break
		}
		name := string(rest[:nul])
		oidStart := space + 1 + nul + 1
		if oidStart+20 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[oidStart : oidStart+20])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		at = oidStart + 20
	}
	return entries
}

// formatWho renders "Name <email> <ts> +0000".
func formatWho(id Identity, ts int64) string {
	return id.Name + " <" + id.Email + "> " + strconv.FormatInt(ts, 10) + " +0000"
}

// parseWho parses "Name <email> <ts> +0000" into an identity and timestamp.
// A missing or unparsable timestamp yields 0, matching parseWhoTs in the TS
// reference (which returns 0 when its regex does not match).
func parseWho(who string) (Identity, int64) {
	lt := strings.IndexByte(who, '<')
	gt := strings.IndexByte(who, '>')
	if lt < 0 || gt < 0 || gt < lt {
		return Identity{Name: strings.TrimSpace(who)}, 0
	}
	name := strings.TrimSpace(who[:lt])
	email := who[lt+1 : gt]
	rest := strings.Fields(who[gt+1:])
	if len(rest) == 0 {
		return Identity{Name: name, Email: email}, 0
	}
	ts, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return Identity{Name: name, Email: email}, 0
	}
	return Identity{Name: name, Email: email}, ts
}

// BuildCommit builds a commit object. The message is used verbatim.
func BuildCommit(in CommitInput) []byte {
	var b strings.Builder
	b.WriteString("tree " + in.Tree + "\n")
	for _, parent := range in.Parents {
		b.WriteString("parent " + parent + "\n")
	}
	b.WriteString("author " + formatWho(in.Author, in.Timestamp) + "\n")
	b.WriteString("committer " + formatWho(in.Committer, in.Timestamp) + "\n")
	b.WriteString("\n" + in.Message)
	return []byte(b.String())
}

// ParseCommit parses a commit object. The single Timestamp is the
// committer's timestamp when present, otherwise the author's (the TS
// reference keeps both; this contract keeps one and both are equal for
// every commit this forge builds).
func ParseCommit(data []byte) ParsedCommit {
	text := string(data)
	split := strings.Index(text, "\n\n")
	header := text
	message := ""
	if split >= 0 {
		header = text[:split]
		message = text[split+2:]
	}
	out := ParsedCommit{Parents: []string{}}
	var authorTs, committerTs int64
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			out.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			out.Parents = append(out.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			out.Author, authorTs = parseWho(line[7:])
		case strings.HasPrefix(line, "committer "):
			out.Committer, committerTs = parseWho(line[10:])
		}
	}
	if committerTs != 0 {
		out.Timestamp = committerTs
	} else {
		out.Timestamp = authorTs
	}
	out.Message = message
	return out
}

// BuildTag builds an annotated tag object.
func BuildTag(in TagInput) []byte {
	text := "object " + in.Object + "\n" +
		"type " + string(in.Type) + "\n" +
		"tag " + in.Tag + "\n" +
		"tagger " + formatWho(in.Tagger, in.Timestamp) + "\n" +
		"\n" + in.Message
	return []byte(text)
}

// ParseTag parses an annotated tag object.
func ParseTag(data []byte) ParsedTag {
	text := string(data)
	split := strings.Index(text, "\n\n")
	header := text
	message := ""
	if split >= 0 {
		header = text[:split]
		message = text[split+2:]
	}
	out := ParsedTag{}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			out.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			out.Type = ObjType(strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "tag "):
			out.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			out.Tagger, out.Timestamp = parseWho(line[7:])
		}
	}
	out.Message = message
	return out
}

// ---------------------------------------------------------- pkt-lines

// EncodePktLine encodes one pkt-line: a 4-hex length (including the header)
// plus the payload. Built in a single allocation. Returns nil when the
// length would exceed the 65520 limit (the TS reference throws).
func EncodePktLine(data []byte) []byte {
	n := len(data) + 4
	if n > 65520 {
		return nil
	}
	out := make([]byte, n)
	hexDigits := "0123456789abcdef"
	out[0] = hexDigits[(n>>12)&0xf]
	out[1] = hexDigits[(n>>8)&0xf]
	out[2] = hexDigits[(n>>4)&0xf]
	out[3] = hexDigits[n&0xf]
	copy(out[4:], data)
	return out
}

// FlushPkt is the flush packet, `0000`.
var FlushPkt = []byte("0000")

// DelimPkt is the delimiter packet, `0001`.
var DelimPkt = []byte("0001")

// EndPkt is the response-end packet, `0002`.
var EndPkt = []byte("0002")

type PktKind int

const (
	PktData PktKind = iota
	PktFlush
	PktDelim
	PktEnd
)

type Pkt struct {
	Kind PktKind
	Data []byte
}

// EncodePktLines encodes a list of packets into pkt-lines in a single
// allocation (total size precomputed).
func EncodePktLines(pkts []Pkt) []byte {
	total := 0
	for _, pkt := range pkts {
		if pkt.Kind == PktData {
			total += len(pkt.Data) + 4
		} else {
			total += 4
		}
	}
	out := make([]byte, 0, total)
	for _, pkt := range pkts {
		switch pkt.Kind {
		case PktFlush:
			out = append(out, FlushPkt...)
		case PktDelim:
			out = append(out, DelimPkt...)
		case PktEnd:
			out = append(out, EndPkt...)
		default:
			out = append(out, EncodePktLine(pkt.Data)...)
		}
	}
	return out
}

// PktLineDecoder is a streaming pkt-line decoder: feed it chunks, pull
// packets out. A packet split across chunks is reassembled; a malformed
// length is recorded and reported by Finish (Next returns false for it).
type PktLineDecoder struct {
	buf []byte
	err error
}

// Push feeds a chunk into the decoder.
func (d *PktLineDecoder) Push(chunk []byte) {
	if len(d.buf) == 0 {
		d.buf = chunk
		return
	}
	d.buf = append(d.buf, chunk...)
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// Next returns the next complete packet, or false when more input is needed
// (or a malformed length was recorded — see Finish).
func (d *PktLineDecoder) Next() (Pkt, bool) {
	if d.err != nil {
		return Pkt{}, false
	}
	if len(d.buf) < 4 {
		return Pkt{}, false
	}
	for i := 0; i < 4; i++ {
		if !isHexDigit(d.buf[i]) {
			d.err = fmt.Errorf("bad pkt-line length %q", string(d.buf[:4]))
			return Pkt{}, false
		}
	}
	n, _ := strconv.ParseUint(string(d.buf[:4]), 16, 32)
	length := int(n)
	switch length {
	case 0:
		d.buf = d.buf[4:]
		return Pkt{Kind: PktFlush}, true
	case 1:
		d.buf = d.buf[4:]
		return Pkt{Kind: PktDelim}, true
	case 2:
		d.buf = d.buf[4:]
		return Pkt{Kind: PktEnd}, true
	}
	if length < 4 || length > 65520 {
		d.err = fmt.Errorf("bad pkt-line length %d", length)
		return Pkt{}, false
	}
	if len(d.buf) < length {
		return Pkt{}, false
	}
	payload := d.buf[4:length]
	d.buf = d.buf[length:]
	return Pkt{Kind: PktData, Data: payload}, true
}

// Finish reports a recorded malformed length, or a trailing partial packet.
func (d *PktLineDecoder) Finish() error {
	if d.err != nil {
		return d.err
	}
	if len(d.buf) > 0 {
		return fmt.Errorf("trailing %d byte(s) after the last pkt-line", len(d.buf))
	}
	return nil
}

// DecodePktLines decodes every packet of a complete buffer.
// It panics on malformed input, mirroring the TS reference's throw.
func DecodePktLines(buf []byte) []Pkt {
	decoder := &PktLineDecoder{}
	decoder.Push(buf)
	out := []Pkt{}
	for {
		pkt, ok := decoder.Next()
		if !ok {
			break
		}
		out = append(out, pkt)
	}
	if err := decoder.Finish(); err != nil {
		panic(err)
	}
	return out
}

// PktLinesAsStrings returns the data packets of a buffer as strings
// (no trailing newline).
func PktLinesAsStrings(buf []byte) []string {
	out := []string{}
	for _, pkt := range DecodePktLines(buf) {
		if pkt.Kind == PktData {
			out = append(out, strings.TrimSuffix(string(pkt.Data), "\n"))
		}
	}
	return out
}

// ---------------------------------------------------------- packfiles

type PackObjectInput struct {
	Type ObjType
	Data []byte
}

// packObjectHeader encodes one object's pack header (type + size varint).
func packObjectHeader(typeCode, size int) []byte {
	var out []byte
	first := (typeCode << 4) | (size & 0x0f)
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	out = append(out, byte(first))
	for size > 0 {
		b := size & 0x7f
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out = append(out, byte(b))
	}
	return out
}

// deflateLevel1 compresses data with zlib at level 1 (matches Node's
// deflateSync level 1 in format; the bytes need not match Node's).
func deflateLevel1(data []byte) []byte {
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, 1)
	if err != nil {
		// Level 1 is always valid; unreachable in practice.
		return nil
	}
	_, _ = w.Write(data)
	_ = w.Close()
	return buf.Bytes()
}

// WritePack writes a packfile (version 2, plain objects, deflate level 1):
// `PACK` + version + count + objects + SHA-1 trailer over the body.
func WritePack(objects []PackObjectInput) []byte {
	var body bytes.Buffer
	body.Grow(12 + len(objects)*64)
	body.WriteString("PACK")
	var hdr [8]byte
	hdr[0], hdr[1], hdr[2], hdr[3] = 0, 0, 0, 2
	n := uint32(len(objects))
	hdr[4] = byte(n >> 24)
	hdr[5] = byte(n >> 16)
	hdr[6] = byte(n >> 8)
	hdr[7] = byte(n)
	body.Write(hdr[:])
	for _, obj := range objects {
		body.Write(packObjectHeader(typeCodes[obj.Type], len(obj.Data)))
		body.Write(deflateLevel1(obj.Data))
	}
	trailer := sha1.Sum(body.Bytes())
	out := make([]byte, 0, body.Len()+20)
	out = append(out, body.Bytes()...)
	out = append(out, trailer[:]...)
	return out
}

// ApplyDelta applies a git delta to a base buffer.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	at := 0
	readVarint := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if at >= len(delta) {
				return 0, fmt.Errorf("delta varint overruns the buffer")
			}
			b := delta[at]
			at++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return value, nil
			}
		}
	}
	sourceSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	targetSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta source size %d does not match the base %d", sourceSize, len(base))
	}
	var out bytes.Buffer
	written := 0
	for at < len(delta) {
		op := delta[at]
		at++
		if op&0x80 != 0 {
			// Copy from the base: offset and size are little-endian bit fields.
			offset := 0
			size := 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if at >= len(delta) {
						return nil, fmt.Errorf("delta copy overruns the buffer")
					}
					offset |= int(delta[at]) << (8 * uint(i))
					at++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if at >= len(delta) {
						return nil, fmt.Errorf("delta copy overruns the buffer")
					}
					size |= int(delta[at]) << (8 * uint(i))
					at++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, fmt.Errorf("delta copy reads past the base")
			}
			out.Write(base[offset : offset+size])
			written += size
		} else if op > 0 {
			// Insert literal bytes.
			if at+int(op) > len(delta) {
				return nil, fmt.Errorf("delta insert overruns the buffer")
			}
			out.Write(delta[at : at+int(op)])
			at += int(op)
			written += int(op)
		} else {
			return nil, fmt.Errorf("delta opcode 0 is reserved")
		}
	}
	result := out.Bytes()
	if written != targetSize || len(result) != targetSize {
		return nil, fmt.Errorf("delta target size %d does not match %d", len(result), targetSize)
	}
	return result, nil
}

type ReadPackResult struct {
	Objects    map[string]GitObject
	DeltaCount int
}

// inflateZlibAll inflates a zlib stream, ignoring any trailing bytes after
// the stream ends (like Node's inflateSync).
func inflateZlibAll(b []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// inflateOneZlib inflates the one zlib stream starting at `at` and says how
// many bytes it consumes. The length comes from the stream's own Adler-32
// trailer: the trailer is the last 4 bytes of the stream, so the stream ends
// where those 4 bytes sit — found by scanning for them, and each candidate
// is verified by re-inflating the prefix (a match inside the deflate data
// inflates truncated and fails, so the first candidate that verifies is the
// real end). Two inflations per object, not a binary search.
func inflateOneZlib(pack []byte, at int) ([]byte, int, error) {
	data, err := inflateZlibAll(pack[at:])
	if err != nil {
		return nil, 0, err
	}
	needle := make([]byte, 4)
	sum := adler32.Checksum(data)
	needle[0] = byte(sum >> 24)
	needle[1] = byte(sum >> 16)
	needle[2] = byte(sum >> 8)
	needle[3] = byte(sum)
	if at+2 > len(pack) {
		return nil, 0, fmt.Errorf("zlib stream end not found")
	}
	from := at + 2
	for {
		rel := bytes.Index(pack[from:], needle)
		if rel < 0 {
			return nil, 0, fmt.Errorf("zlib stream end not found")
		}
		idx := from + rel
		end := idx + 4
		check, err := inflateZlibAll(pack[at:end])
		if err == nil && bytes.Equal(check, data) {
			return data, end - at, nil
		}
		// A match inside the deflate data: the truncated prefix cannot inflate.
		from = idx + 1
	}
}

// ReadPack reads a packfile: verifies the header, version and trailer
// checksum, inflates every entry, resolves OFS_DELTA and REF_DELTA against
// the pack itself or baseOf (a lookup for thin-pack bases the caller holds),
// and verifies each object's id and declared size. It returns an error on
// any corruption.
func ReadPack(pack []byte, baseOf func(oid string) *GitObject) (ReadPackResult, error) {
	if len(pack) < 32 {
		return ReadPackResult{}, fmt.Errorf("packfile too short")
	}
	if string(pack[0:4]) != "PACK" {
		return ReadPackResult{}, fmt.Errorf("packfile does not start with PACK")
	}
	version := uint32(pack[4])<<24 | uint32(pack[5])<<16 | uint32(pack[6])<<8 | uint32(pack[7])
	if version != 2 && version != 3 {
		return ReadPackResult{}, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(uint32(pack[8])<<24 | uint32(pack[9])<<16 | uint32(pack[10])<<8 | uint32(pack[11]))
	trailer := pack[len(pack)-20:]
	actual := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(actual[:], trailer) {
		return ReadPackResult{}, fmt.Errorf("pack trailer checksum does not match its bytes")
	}
	objects := make(map[string]GitObject)
	byOffset := make(map[int]string)
	deltaCount := 0
	at := 12
	end := len(pack) - 20
	for index := 0; index < count; index++ {
		entryOffset := at
		if at >= end {
			return ReadPackResult{}, fmt.Errorf("packfile ends before its declared object count")
		}
		// Type + size varint.
		b := pack[at]
		at++
		typeCode := int(b>>4) & 0x07
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if at >= end {
				return ReadPackResult{}, fmt.Errorf("pack object header overruns the pack")
			}
			b = pack[at]
			at++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		var typ ObjType
		var data []byte
		if typeCode == 6 {
			// OFS_DELTA: a base offset encoded as a negative-offset varint.
			if at >= end {
				return ReadPackResult{}, fmt.Errorf("ofs-delta overruns the pack")
			}
			b = pack[at]
			at++
			offset := int(b & 0x7f)
			for b&0x80 != 0 {
				if at >= end {
					return ReadPackResult{}, fmt.Errorf("ofs-delta overruns the pack")
				}
				b = pack[at]
				at++
				offset = ((offset + 1) << 7) | int(b&0x7f)
			}
			baseOffset := entryOffset - offset
			baseOID, ok := byOffset[baseOffset]
			if !ok {
				return ReadPackResult{}, fmt.Errorf("ofs-delta base is not in the pack")
			}
			base, ok := objects[baseOID]
			if !ok {
				return ReadPackResult{}, fmt.Errorf("ofs-delta base object is missing")
			}
			inflated, consumed, err := inflateOneZlib(pack, at)
			if err != nil {
				return ReadPackResult{}, err
			}
			at += consumed
			// The header size of a delta is the delta data's size, not the
			// result's: check the inflated delta, before applying it.
			if len(inflated) != size {
				return ReadPackResult{}, fmt.Errorf("pack delta size %d does not match its header %d", len(inflated), size)
			}
			data, err = ApplyDelta(base.Data, inflated)
			if err != nil {
				return ReadPackResult{}, err
			}
			typ = base.Type
			deltaCount++
		} else if typeCode == 7 {
			// REF_DELTA: a 20-byte base id.
			if at+20 > end {
				return ReadPackResult{}, fmt.Errorf("ref-delta overruns the pack")
			}
			baseOID := hex.EncodeToString(pack[at : at+20])
			at += 20
			base, ok := objects[baseOID]
			if !ok && baseOf != nil {
				if b := baseOf(baseOID); b != nil {
					base = *b
					ok = true
				}
			}
			if !ok {
				return ReadPackResult{}, fmt.Errorf("ref-delta base %s is not available", baseOID)
			}
			inflated, consumed, err := inflateOneZlib(pack, at)
			if err != nil {
				return ReadPackResult{}, err
			}
			at += consumed
			if len(inflated) != size {
				return ReadPackResult{}, fmt.Errorf("pack delta size %d does not match its header %d", len(inflated), size)
			}
			data, err = ApplyDelta(base.Data, inflated)
			if err != nil {
				return ReadPackResult{}, err
			}
			typ = base.Type
			deltaCount++
		} else {
			var ok bool
			typ, ok = codeTypes[typeCode]
			if !ok {
				return ReadPackResult{}, fmt.Errorf("unknown pack object type %d", typeCode)
			}
			inflated, consumed, err := inflateOneZlib(pack, at)
			if err != nil {
				return ReadPackResult{}, err
			}
			at += consumed
			// A plain object: the header size is the object's own size.
			if len(inflated) != size {
				return ReadPackResult{}, fmt.Errorf("pack object size %d does not match its header %d", len(inflated), size)
			}
			data = inflated
		}
		oid := HashObject(typ, data)
		if _, exists := objects[oid]; !exists {
			objects[oid] = GitObject{Type: typ, Data: data}
		}
		byOffset[entryOffset] = oid
	}
	return ReadPackResult{Objects: objects, DeltaCount: deltaCount}, nil
}

// ---------------------------------------------------------- reachability

type FetchPlan struct {
	Objects   []GitObject
	Shallow   []string
	Unshallow []string
}

// isOIDHex reports whether s is a 40-hex-char object id. (A local check so
// this file does not depend on util.go's IsOID stub being filled in yet;
// it implements the same rule.)
func isOIDHex(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}

// PlanFetch computes the fetch plan for one upload-pack request. `wants`
// are the tips, `haves` the commits the client holds, `depth` the shallow
// limit (nil = the whole history), `priorShallow` the client's current
// shallow boundaries.
//
// The pack holds exactly the objects reachable from the wants within the
// depth that the client does not already hold: a commit the client holds is
// excluded, but the walk passes through it (a deepen re-fetches ancestors
// of a held want), and a held commit's tree and blobs are excluded too.
// Boundary commits (at exactly the depth limit, with parents) are reported
// as shallow; previously-shallow commits that are now fully covered are
// reported as unshallow.
func PlanFetch(lookup func(oid string) *GitObject, wants, haves []string, depth *int, priorShallow []string) FetchPlan {
	finite := depth != nil
	limit := 0
	if finite {
		limit = *depth
	}
	// 1. The want closure, with depth. Insertion order is kept (the TS
	// reference's Map preserves it) so the pack order is deterministic.
	included := make(map[string]int)
	includedOrder := []string{}
	type queueItem struct {
		oid string
		d   int
	}
	queue := []queueItem{}
	qhead := 0
	for _, want := range wants {
		if isOIDHex(want) && want != ZeroOID {
			if _, ok := included[want]; !ok {
				included[want] = 1
				includedOrder = append(includedOrder, want)
				queue = append(queue, queueItem{oid: want, d: 1})
			}
		}
	}
	for qhead < len(queue) {
		item := queue[qhead]
		qhead++
		if finite && item.d >= limit {
			continue
		}
		obj := lookup(item.oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		for _, parent := range ParseCommit(obj.Data).Parents {
			if _, ok := included[parent]; !ok {
				included[parent] = item.d + 1
				includedOrder = append(includedOrder, parent)
				queue = append(queue, queueItem{oid: parent, d: item.d + 1})
			}
		}
	}
	// 2. The held closure: from the haves, never past a prior shallow line.
	held := make(map[string]bool)
	priorSet := make(map[string]bool, len(priorShallow))
	for _, oid := range priorShallow {
		priorSet[oid] = true
	}
	hqueue := []string{}
	hhead := 0
	for _, have := range haves {
		if isOIDHex(have) && have != ZeroOID {
			if !held[have] {
				held[have] = true
				hqueue = append(hqueue, have)
			}
		}
	}
	for hhead < len(hqueue) {
		oid := hqueue[hhead]
		hhead++
		if priorSet[oid] {
			continue // a shallow boundary: held, but not its parents
		}
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		for _, parent := range ParseCommit(obj.Data).Parents {
			if !held[parent] {
				held[parent] = true
				hqueue = append(hqueue, parent)
			}
		}
	}
	// 3. The pack commits: included, not held.
	packCommits := []string{}
	for _, oid := range includedOrder {
		if !held[oid] {
			packCommits = append(packCommits, oid)
		}
	}
	// 4. The held objects (a held commit's whole tree), for exclusion.
	heldObjects := make(map[string]bool)
	collectTreeObjects := func(rootOID string, into map[string]bool) {
		seen := make(map[string]bool)
		var walk func(treeOID string)
		walk = func(treeOID string) {
			if seen[treeOID] {
				return
			}
			seen[treeOID] = true
			into[treeOID] = true
			tree := lookup(treeOID)
			if tree == nil || tree.Type != ObjTree {
				return
			}
			for _, entry := range ParseTree(tree.Data) {
				if entry.Mode == "40000" {
					walk(entry.OID)
				} else if entry.Mode != "160000" {
					into[entry.OID] = true
				}
			}
		}
		walk(rootOID)
	}
	for oid := range held {
		heldObjects[oid] = true
		obj := lookup(oid)
		if obj != nil && obj.Type == ObjCommit {
			collectTreeObjects(ParseCommit(obj.Data).Tree, heldObjects)
		}
	}
	// 5. The pack objects: each pack commit plus its tree, minus held objects.
	// Insertion order is kept so the pack order is deterministic.
	objects := make(map[string]GitObject)
	order := []string{}
	setObject := func(oid string, obj GitObject) {
		if _, ok := objects[oid]; !ok {
			order = append(order, oid)
		}
		objects[oid] = obj
	}
	addTree := func(treeOID string) {
		seen := make(map[string]bool)
		var walk func(oid string)
		walk = func(oid string) {
			if seen[oid] || heldObjects[oid] {
				return
			}
			seen[oid] = true
			tree := lookup(oid)
			if tree == nil {
				return
			}
			setObject(oid, *tree)
			for _, entry := range ParseTree(tree.Data) {
				if entry.Mode == "40000" {
					walk(entry.OID)
				} else if entry.Mode != "160000" && !heldObjects[entry.OID] {
					if blob := lookup(entry.OID); blob != nil {
						setObject(entry.OID, *blob)
					}
				}
			}
		}
		walk(treeOID)
	}
	for _, oid := range packCommits {
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		setObject(oid, *obj)
		if obj.Type == ObjCommit {
			addTree(ParseCommit(obj.Data).Tree)
		} else if obj.Type == ObjTag {
			target := ParseTag(obj.Data).Object
			if peeled := lookup(target); peeled != nil && peeled.Type == ObjCommit {
				setObject(target, *peeled)
				addTree(ParseCommit(peeled.Data).Tree)
			}
		}
	}
	// 6. Shallow / unshallow.
	shallow := []string{}
	if finite {
		for _, oid := range includedOrder {
			if included[oid] != limit {
				continue
			}
			obj := lookup(oid)
			if obj == nil || obj.Type != ObjCommit {
				continue
			}
			if len(ParseCommit(obj.Data).Parents) > 0 {
				shallow = append(shallow, oid)
			}
		}
	}
	boundary := make(map[string]bool, len(shallow))
	for _, oid := range shallow {
		boundary[oid] = true
	}
	unshallow := []string{}
	for _, oid := range priorShallow {
		if _, ok := included[oid]; ok && !boundary[oid] {
			unshallow = append(unshallow, oid)
		}
	}
	planObjects := make([]GitObject, 0, len(order))
	for _, oid := range order {
		planObjects = append(planObjects, objects[oid])
	}
	return FetchPlan{Objects: planObjects, Shallow: shallow, Unshallow: unshallow}
}

// ReachableObjects returns every object reachable from `tips`
// (commits, trees, blobs), by id.
func ReachableObjects(lookup func(oid string) *GitObject, tips []string) map[string]bool {
	out := make(map[string]bool)
	queue := []string{}
	qhead := 0
	for _, tip := range tips {
		if isOIDHex(tip) && tip != ZeroOID && !out[tip] {
			out[tip] = true
			queue = append(queue, tip)
		}
	}
	for qhead < len(queue) {
		oid := queue[qhead]
		qhead++
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case ObjCommit:
			commit := ParseCommit(obj.Data)
			if !out[commit.Tree] {
				out[commit.Tree] = true
				queue = append(queue, commit.Tree)
			}
			for _, parent := range commit.Parents {
				if !out[parent] {
					out[parent] = true
					queue = append(queue, parent)
				}
			}
		case ObjTree:
			for _, entry := range ParseTree(obj.Data) {
				if entry.Mode == "160000" {
					continue
				}
				if !out[entry.OID] {
					out[entry.OID] = true
					queue = append(queue, entry.OID)
				}
			}
		case ObjTag:
			target := ParseTag(obj.Data).Object
			if !out[target] {
				out[target] = true
				queue = append(queue, target)
			}
		}
	}
	return out
}
