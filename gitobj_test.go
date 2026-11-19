package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"reflect"
	"testing"
)

// --- helpers ---------------------------------------------------------------

type memStore struct {
	objects map[string]GitObject
}

func newMemStore() *memStore { return &memStore{objects: map[string]GitObject{}} }

func (m *memStore) put(typ ObjType, data []byte) string {
	oid := HashObject(typ, data)
	m.objects[oid] = GitObject{Type: typ, Data: data}
	return oid
}

func (m *memStore) lookup(oid string) *GitObject {
	if obj, ok := m.objects[oid]; ok {
		o := obj
		return &o
	}
	return nil
}

func oidSet(objs []GitObject) map[string]bool {
	out := map[string]bool{}
	for _, o := range objs {
		out[HashObject(o.Type, o.Data)] = true
	}
	return out
}

// --- hash / tree / commit / tag -------------------------------------------

func TestHashObjectEmptyBlob(t *testing.T) {
	// Well-known git id of the empty blob.
	if got := HashObject(ObjBlob, []byte{}); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Fatalf("empty blob oid = %s", got)
	}
}

func TestTreeBuildParseRoundTrip(t *testing.T) {
	blobOID := HashObject(ObjBlob, []byte("hello"))
	subOID := HashObject(ObjTree, []byte("sub"))
	// Deliberately unsorted input, incl. a directory that must sort as "dir/".
	entries := []TreeEntry{
		{Mode: "100644", Name: "z.txt", OID: blobOID},
		{Mode: "40000", Name: "dir", OID: subOID},
		{Mode: "100644", Name: "a.txt", OID: blobOID},
		{Mode: "160000", Name: "submod", OID: blobOID},
	}
	data := BuildTree(entries)
	got := ParseTree(data)
	// Git order: a.txt, dir (as "dir/"), submod, z.txt.
	wantNames := []string{"a.txt", "dir", "submod", "z.txt"}
	if len(got) != len(wantNames) {
		t.Fatalf("parsed %d entries, want %d: %+v", len(got), len(wantNames), got)
	}
	for i, name := range wantNames {
		if got[i].Name != name {
			t.Fatalf("entry %d name = %q, want %q", i, got[i].Name, name)
		}
	}
	if got[1].Mode != "40000" || got[1].OID != subOID {
		t.Fatalf("dir entry = %+v", got[1])
	}
	if got[3].OID != blobOID {
		t.Fatalf("z.txt oid mismatch")
	}
	// BuildTree must not mutate its input.
	if entries[0].Name != "z.txt" {
		t.Fatalf("BuildTree mutated its input")
	}
}

func TestCommitBuildParseRoundTrip(t *testing.T) {
	in := CommitInput{
		Tree:      HashObject(ObjTree, []byte{}),
		Parents:   []string{HashObject(ObjCommit, []byte("p1")), HashObject(ObjCommit, []byte("p2"))},
		Message:   "first line\n\nbody text",
		Author:    Identity{Name: "Ada", Email: "ada@example.invalid"},
		Committer: Identity{Name: "Bo", Email: "bo@example.invalid"},
		Timestamp: 1735689600,
	}
	data := BuildCommit(in)
	want := "tree " + in.Tree + "\nparent " + in.Parents[0] + "\nparent " + in.Parents[1] + "\n" +
		"author Ada <ada@example.invalid> 1735689600 +0000\n" +
		"committer Bo <bo@example.invalid> 1735689600 +0000\n\nfirst line\n\nbody text"
	if string(data) != want {
		t.Fatalf("commit bytes = %q\nwant %q", data, want)
	}
	parsed := ParseCommit(data)
	if parsed.Tree != in.Tree || parsed.Message != in.Message {
		t.Fatalf("parsed commit = %+v", parsed)
	}
	if !reflect.DeepEqual(parsed.Parents, in.Parents) {
		t.Fatalf("parents = %v, want %v", parsed.Parents, in.Parents)
	}
	if parsed.Author != in.Author || parsed.Committer != in.Committer {
		t.Fatalf("author=%+v committer=%+v", parsed.Author, parsed.Committer)
	}
	if parsed.Timestamp != 1735689600 {
		t.Fatalf("timestamp = %d", parsed.Timestamp)
	}
}

func TestTagBuildParseRoundTrip(t *testing.T) {
	target := HashObject(ObjCommit, []byte("c"))
	in := TagInput{
		Object: target, Type: ObjCommit, Tag: "v1.0",
		Tagger:    Identity{Name: "Ada", Email: "ada@example.invalid"},
		Timestamp: 1735689600, Message: "release one",
	}
	data := BuildTag(in)
	parsed := ParseTag(data)
	if parsed.Object != target || parsed.Type != ObjCommit || parsed.Tag != "v1.0" {
		t.Fatalf("parsed tag = %+v", parsed)
	}
	if parsed.Tagger != in.Tagger || parsed.Timestamp != 1735689600 || parsed.Message != "release one" {
		t.Fatalf("parsed tag = %+v", parsed)
	}
}

// --- pkt-lines --------------------------------------------------------------

func TestPktLineRoundTrip(t *testing.T) {
	pkts := []Pkt{
		{Kind: PktData, Data: []byte("hello\n")},
		{Kind: PktFlush},
		{Kind: PktData, Data: []byte("world")},
		{Kind: PktDelim},
		{Kind: PktEnd},
	}
	enc := EncodePktLines(pkts)
	if string(enc) != "000ahello\n00000009world00010002" {
		t.Fatalf("encoded = %q", enc)
	}
	// Streaming decoder fed one byte at a time.
	dec := &PktLineDecoder{}
	var got []Pkt
	for _, b := range enc {
		dec.Push([]byte{b})
		for {
			p, ok := dec.Next()
			if !ok {
				break
			}
			got = append(got, p)
		}
	}
	if err := dec.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(got) != len(pkts) {
		t.Fatalf("decoded %d packets, want %d", len(got), len(pkts))
	}
	for i, p := range pkts {
		if got[i].Kind != p.Kind || !bytes.Equal(got[i].Data, p.Data) {
			t.Fatalf("packet %d = %+v, want %+v", i, got[i], p)
		}
	}
	// Whole-buffer helpers.
	decoded := DecodePktLines(enc)
	if len(decoded) != len(pkts) {
		t.Fatalf("DecodePktLines = %d packets", len(decoded))
	}
	strs := PktLinesAsStrings(enc)
	if !reflect.DeepEqual(strs, []string{"hello", "world"}) {
		t.Fatalf("PktLinesAsStrings = %v", strs)
	}
	// Trailing partial packet must be reported by Finish.
	dec2 := &PktLineDecoder{}
	dec2.Push([]byte("000ahe")) // declares 10 bytes, only 6 present
	if _, ok := dec2.Next(); ok {
		t.Fatalf("partial packet returned a packet")
	}
	if err := dec2.Finish(); err == nil {
		t.Fatalf("Finish accepted a trailing partial packet")
	}
	// Malformed length must be reported by Finish.
	dec3 := &PktLineDecoder{}
	dec3.Push([]byte("zzzz"))
	if _, ok := dec3.Next(); ok {
		t.Fatalf("malformed length returned a packet")
	}
	if err := dec3.Finish(); err == nil {
		t.Fatalf("Finish accepted a malformed length")
	}
}

// --- deltas -----------------------------------------------------------------

func TestApplyDeltaVectors(t *testing.T) {
	base := []byte("hello world, hello git")
	// Literal-only delta: source size varint, target size varint, insert op.
	delta := []byte{byte(len(base)), 5, 5, 'h', 'e', 'l', 'l', 'o'}
	out, err := ApplyDelta(base, delta)
	if err != nil || string(out) != "hello" {
		t.Fatalf("literal delta = %q, err %v", out, err)
	}
	// Copy + insert: copy 5 bytes at offset 6 ("world"), insert ",!".
	// varints: source 22, target 7. Copy op: 0x80|0x01(offset byte)|0x10(size byte).
	delta2 := []byte{22, 7, 0x91, 6, 5, 2, ',', '!'}
	out2, err := ApplyDelta(base, delta2)
	if err != nil || string(out2) != "world,!" {
		t.Fatalf("copy delta = %q, err %v", out2, err)
	}
	// Copy with size 0 means 0x10000: must fail (reads past base).
	delta3 := []byte{22, 3, 0x90, 0}
	if _, err := ApplyDelta(base, delta3); err == nil {
		t.Fatalf("oversized copy delta accepted")
	}
	// Wrong source size.
	if _, err := ApplyDelta(base, []byte{21, 1, 1, 'x'}); err == nil {
		t.Fatalf("wrong source size accepted")
	}
	// Opcode 0 is reserved.
	if _, err := ApplyDelta(base, []byte{22, 0, 0}); err == nil {
		t.Fatalf("opcode 0 accepted")
	}
	// Wrong target size.
	if _, err := ApplyDelta(base, []byte{22, 9, 5, 'h', 'e', 'l', 'l', 'o'}); err == nil {
		t.Fatalf("wrong target size accepted")
	}
}

// --- packs ------------------------------------------------------------------

func TestWriteReadPackRoundTrip(t *testing.T) {
	blob := []byte("pack me please, this is a blob with some repeated repeated repeated text")
	tree := BuildTree([]TreeEntry{{Mode: "100644", Name: "f.txt", OID: HashObject(ObjBlob, blob)}})
	commit := BuildCommit(CommitInput{
		Tree: HashObject(ObjTree, tree), Message: "c1",
		Author: Identity{Name: "Ada", Email: "a@x"}, Committer: Identity{Name: "Ada", Email: "a@x"},
		Timestamp: 1000,
	})
	pack := WritePack([]PackObjectInput{
		{Type: ObjBlob, Data: blob},
		{Type: ObjTree, Data: tree},
		{Type: ObjCommit, Data: commit},
	})
	// Trailer must be SHA-1 of the body.
	body, trailer := pack[:len(pack)-20], pack[len(pack)-20:]
	sum := sha1.Sum(body)
	if !bytes.Equal(sum[:], trailer) {
		t.Fatalf("trailer is not SHA-1 of the body")
	}
	res, err := ReadPack(pack, nil)
	if err != nil {
		t.Fatalf("ReadPack: %v", err)
	}
	if res.DeltaCount != 0 || len(res.Objects) != 3 {
		t.Fatalf("objects=%d deltaCount=%d", len(res.Objects), res.DeltaCount)
	}
	for typ, data := range map[ObjType][]byte{ObjBlob: blob, ObjTree: tree, ObjCommit: commit} {
		oid := HashObject(typ, data)
		obj, ok := res.Objects[oid]
		if !ok || obj.Type != typ || !bytes.Equal(obj.Data, data) {
			t.Fatalf("object %s missing or wrong: %+v", oid, obj)
		}
	}
	// Corrupt one body byte: trailer check must fail.
	bad := append([]byte{}, pack...)
	bad[20] ^= 0xff
	if _, err := ReadPack(bad, nil); err == nil {
		t.Fatalf("corrupted pack accepted")
	}
	// Bad magic / short pack.
	if _, err := ReadPack([]byte("NOPE"), nil); err == nil {
		t.Fatalf("short pack accepted")
	}
}

// encodeDelta builds a delta that turns base into target using one copy of
// the common prefix (if any) plus a literal tail.
func encodeDelta(t *testing.T, base, target []byte) []byte {
	t.Helper()
	varint := func(v int) []byte {
		var out []byte
		for {
			b := byte(v & 0x7f)
			v >>= 7
			if v != 0 {
				b |= 0x80
			}
			out = append(out, b)
			if v == 0 {
				return out
			}
		}
	}
	// Common prefix length.
	prefix := 0
	for prefix < len(base) && prefix < len(target) && base[prefix] == target[prefix] {
		prefix++
	}
	delta := append(varint(len(base)), varint(len(target))...)
	if prefix > 0 {
		// Copy op for offset 0, size prefix (size fits in one byte here).
		delta = append(delta, 0x80|0x10, byte(prefix))
	}
	rest := target[prefix:]
	for len(rest) > 0 {
		n := len(rest)
		if n > 127 {
			n = 127
		}
		delta = append(delta, byte(n))
		delta = append(delta, rest[:n]...)
		rest = rest[n:]
	}
	return delta
}

func deflate(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// encodeOfsOffset encodes a negative-offset varint (git OFS_DELTA form).
func encodeOfsOffset(n int) []byte {
	buf := []byte{byte(n & 0x7f)}
	n >>= 7
	for n > 0 {
		n--
		buf = append([]byte{byte(n&0x7f) | 0x80}, buf...)
		n >>= 7
	}
	return buf
}

func packHeaderFor(t *testing.T, typeCode, size int) []byte {
	t.Helper()
	// Mirror packObjectHeader via WritePack of a probe is awkward; rebuild
	// the varint directly (same encoding as git).
	first := (typeCode << 4) | (size & 0x0f)
	size >>= 4
	out := []byte{}
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

func finishPack(t *testing.T, body []byte) []byte {
	t.Helper()
	sum := sha1.Sum(body)
	return append(append([]byte{}, body...), sum[:]...)
}

func packPrefix(count int) []byte {
	out := []byte("PACK")
	out = append(out, 0, 0, 0, 2)
	out = append(out, byte(count>>24), byte(count>>16), byte(count>>8), byte(count))
	return out
}

func TestReadPackOfsDelta(t *testing.T) {
	base := []byte("hello world, this is the base blob content")
	target := []byte("hello world, this is the NEW blob content!!")
	delta := encodeDelta(t, base, target)
	// Pack: [base blob][OFS_DELTA -> base]. The delta entry's offset is
	// measured back from its own start to the base entry's start.
	body := packPrefix(2)
	baseEntry := append(packHeaderFor(t, 3, len(base)), deflate(t, base)...)
	body = append(body, baseEntry...)
	deltaEntryOffset := len(body)
	ofs := deltaEntryOffset - 12 // base entry starts at offset 12
	deltaEntry := append(packHeaderFor(t, 6, len(delta)), encodeOfsOffset(ofs)...)
	deltaEntry = append(deltaEntry, deflate(t, delta)...)
	body = append(body, deltaEntry...)
	pack := finishPack(t, body)
	res, err := ReadPack(pack, nil)
	if err != nil {
		t.Fatalf("ReadPack OFS_DELTA: %v", err)
	}
	if res.DeltaCount != 1 {
		t.Fatalf("DeltaCount = %d, want 1", res.DeltaCount)
	}
	oid := HashObject(ObjBlob, target)
	obj, ok := res.Objects[oid]
	if !ok || !bytes.Equal(obj.Data, target) {
		t.Fatalf("delta result missing/wrong: %+v", obj)
	}
}

func TestReadPackRefDeltaWithBaseOf(t *testing.T) {
	base := []byte("abcdefghij0123456789")
	target := []byte("abcdefghij0123456789-EXTRA")
	baseOID := HashObject(ObjBlob, base)
	delta := encodeDelta(t, base, target)
	body := packPrefix(1)
	entry := append(packHeaderFor(t, 7, len(delta)), []byte{}...)
	raw, _ := hex.DecodeString(baseOID)
	entry = append(entry, raw...)
	entry = append(entry, deflate(t, delta)...)
	body = append(body, entry...)
	pack := finishPack(t, body)
	baseObj := GitObject{Type: ObjBlob, Data: base}
	res, err := ReadPack(pack, func(oid string) *GitObject {
		if oid == baseOID {
			return &baseObj
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReadPack REF_DELTA: %v", err)
	}
	if res.DeltaCount != 1 {
		t.Fatalf("DeltaCount = %d, want 1", res.DeltaCount)
	}
	oid := HashObject(ObjBlob, target)
	if obj, ok := res.Objects[oid]; !ok || !bytes.Equal(obj.Data, target) {
		t.Fatalf("ref-delta result missing/wrong: %+v", obj)
	}
	// Without baseOf the same pack must fail.
	if _, err := ReadPack(pack, nil); err == nil {
		t.Fatalf("ref-delta without a base was accepted")
	}
}

// --- fetch plan / reachability ----------------------------------------------

func buildChain(t *testing.T) (*memStore, string, string, string) {
	t.Helper()
	m := newMemStore()
	blob1 := m.put(ObjBlob, []byte("one"))
	blob2 := m.put(ObjBlob, []byte("two"))
	blob3 := m.put(ObjBlob, []byte("three"))
	tree1 := m.put(ObjTree, BuildTree([]TreeEntry{{Mode: "100644", Name: "f", OID: blob1}}))
	tree2 := m.put(ObjTree, BuildTree([]TreeEntry{{Mode: "100644", Name: "f", OID: blob2}}))
	tree3 := m.put(ObjTree, BuildTree([]TreeEntry{{Mode: "100644", Name: "f", OID: blob3}}))
	mk := func(tree string, parents []string, msg string) string {
		return m.put(ObjCommit, BuildCommit(CommitInput{
			Tree: tree, Parents: parents, Message: msg,
			Author: Identity{Name: "Ada", Email: "a@x"}, Committer: Identity{Name: "Ada", Email: "a@x"},
			Timestamp: 1000,
		}))
	}
	c1 := mk(tree1, nil, "c1")
	c2 := mk(tree2, []string{c1}, "c2")
	c3 := mk(tree3, []string{c2}, "c3")
	return m, c1, c2, c3
}

func TestPlanFetchBasic(t *testing.T) {
	m, c1, c2, c3 := buildChain(t)
	plan := PlanFetch(m.lookup, []string{c3}, []string{c1}, nil, nil)
	got := oidSet(plan.Objects)
	// c1 is held, so its commit/tree/blob are excluded; c2, c3 and their
	// trees/blobs are sent.
	want := map[string]bool{}
	for _, oid := range []string{c2, c3} {
		want[oid] = true
		commit := ParseCommit(m.objects[oid].Data)
		want[commit.Tree] = true
		for _, e := range ParseTree(m.objects[commit.Tree].Data) {
			want[e.OID] = true
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan objects = %v\nwant %v", got, want)
	}
	if len(plan.Shallow) != 0 || len(plan.Unshallow) != 0 {
		t.Fatalf("shallow=%v unshallow=%v, want empty", plan.Shallow, plan.Unshallow)
	}
	// Held commit's tree must not leak in even though c2's tree differs.
	if got[c1] {
		t.Fatalf("held commit c1 leaked into the plan")
	}
}

func TestPlanFetchDepth(t *testing.T) {
	m, _, _, c3 := buildChain(t)
	depth := 1
	plan := PlanFetch(m.lookup, []string{c3}, nil, &depth, nil)
	// Depth 1: only c3 (plus its tree/blob); c3 is a boundary with parents.
	if !reflect.DeepEqual(plan.Shallow, []string{c3}) {
		t.Fatalf("shallow = %v, want [%s]", plan.Shallow, c3)
	}
	got := oidSet(plan.Objects)
	commit := ParseCommit(m.objects[c3].Data)
	want := map[string]bool{c3: true, commit.Tree: true}
	for _, e := range ParseTree(m.objects[commit.Tree].Data) {
		want[e.OID] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("depth-1 objects = %v, want %v", got, want)
	}
	// Depth 2 with c3 previously shallow: c3 is now covered (depth 2
	// includes c2, so c3 is no longer a boundary) -> unshallow [c3],
	// shallow [c2].
	depth = 2
	plan2 := PlanFetch(m.lookup, []string{c3}, nil, &depth, []string{c3})
	// Find c2's oid from the chain (second commit): it is the only shallow
	// entry and must be a parent of c3.
	if len(plan2.Shallow) != 1 || plan2.Shallow[0] != ParseCommit(m.objects[c3].Data).Parents[0] {
		t.Fatalf("depth-2 shallow = %v", plan2.Shallow)
	}
	if !reflect.DeepEqual(plan2.Unshallow, []string{c3}) {
		t.Fatalf("depth-2 unshallow = %v, want [%s]", plan2.Unshallow, c3)
	}
}

func TestReachableObjects(t *testing.T) {
	m, c1, c2, c3 := buildChain(t)
	reach := ReachableObjects(m.lookup, []string{c3})
	// All 3 commits, 3 trees, 3 blobs.
	if len(reach) != 9 {
		t.Fatalf("reachable = %d objects, want 9", len(reach))
	}
	for _, oid := range []string{c1, c2, c3} {
		if !reach[oid] {
			t.Fatalf("reachable missing %s", oid)
		}
	}
	// Zero OID and garbage tips are ignored.
	if r := ReachableObjects(m.lookup, []string{ZeroOID, "not-an-oid"}); len(r) != 0 {
		t.Fatalf("garbage tips gave %v", r)
	}
	// A gitlink entry (mode 160000) is not followed.
	blob := m.put(ObjBlob, []byte("x"))
	tree := m.put(ObjTree, BuildTree([]TreeEntry{
		{Mode: "160000", Name: "sub", OID: blob},
		{Mode: "100644", Name: "f", OID: blob},
	}))
	if r := ReachableObjects(m.lookup, []string{tree}); !r[tree] || !r[blob] {
		t.Fatalf("tree reachability wrong: %v", r)
	}
}
