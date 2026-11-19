// pack.go — the git object model + packfile codec + pkt-lines.
//
// Ported from the Node reference (store.ts) into idiomatic Go, stdlib only.
// All hashes are SHA-1 over "type len\0data", matching git.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// object model
// ---------------------------------------------------------------------------

// TreeEntry is one entry of a git tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// Commit is a parsed git commit object (headers + message).
type Commit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// Tag is a parsed git tag object.
type Tag struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

// HashObject returns the SHA-1 object id of "type len\0data".
func HashObject(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// IsObjectID reports whether s is a 40-hex-char object id.
func IsObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ParseTree parses a raw tree object into its entries.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has a truncated object id")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries, nil
}

// compareTreeNames orders entries the way git does: directories sort as if
// their name had a trailing slash.
func compareTreeNames(a, b TreeEntry) int {
	an := a.Name
	if a.Mode == "40000" {
		an += "/"
	}
	bn := b.Name
	if b.Mode == "40000" {
		bn += "/"
	}
	return strings.Compare(an, bn)
}

// BuildTree serializes entries into a raw tree object (sorted git order).
func BuildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return compareTreeNames(sorted[i], sorted[j]) < 0
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ParseCommit parses a raw commit object.
func ParseCommit(data []byte) (Commit, error) {
	var c Commit
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
		c.Message = string(data[blank+2:])
	}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			c.Tree = value
		case "parent":
			c.Parents = append(c.Parents, value)
		case "author":
			c.Author = value
		case "committer":
			c.Committer = value
		}
	}
	return c, nil
}

// BuildCommit serializes a commit. Empty author/committer default to the
// viewer identity; the timestamp is always FIXED_EPOCH (deterministic).
func BuildCommit(c Commit) []byte {
	author := c.Author
	if author == "" {
		author = ViewerName + " <" + ViewerEmail + ">"
	}
	committer := c.Committer
	if committer == "" {
		committer = author
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", c.Tree)
	for _, p := range c.Parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s %d +0000\n", author, FIXED_EPOCH)
	fmt.Fprintf(&b, "committer %s %d +0000\n", committer, FIXED_EPOCH)
	b.WriteString("\n")
	b.WriteString(c.Message)
	return []byte(b.String())
}

// ParseTag parses a raw tag object.
func ParseTag(data []byte) Tag {
	var t Tag
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
		t.Message = string(data[blank+2:])
	}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch key {
		case "object":
			t.Object = value
		case "type":
			t.Type = value
		case "tag":
			t.Tag = value
		case "tagger":
			t.Tagger = value
		}
	}
	return t
}

// BuildTag serializes a tag object (deterministic timestamp).
func BuildTag(t Tag) []byte {
	tagger := t.Tagger
	if tagger == "" {
		tagger = ViewerName + " <" + ViewerEmail + ">"
	}
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s %d +0000\n\n%s",
		t.Object, t.Type, t.Tag, tagger, FIXED_EPOCH, t.Message))
}

// CollectReachable returns every object reachable from roots
// (commit -> tree + parents, tag -> target, tree -> entries, skipping
// submodule entries). With allowMissing, absent objects are skipped;
// otherwise a missing object is an error.
func CollectReachable(roots []string, lookup func(oid string) *Object, allowMissing bool) (map[string]bool, error) {
	seen := make(map[string]bool)
	queue := make([]string, 0, len(roots))
	for _, r := range roots {
		if IsObjectID(r) {
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			if allowMissing {
				continue
			}
			return nil, fmt.Errorf("object %s is missing from the store", oid)
		}
		seen[oid] = true
		switch obj.Type {
		case "commit":
			c, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, c.Tree)
			queue = append(queue, c.Parents...)
		case "tag":
			queue = append(queue, ParseTag(obj.Data).Object)
		case "tree":
			entries, err := ParseTree(obj.Data)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				queue = append(queue, e.OID)
			}
		}
	}
	return seen, nil
}

// ---------------------------------------------------------------------------
// deltas
// ---------------------------------------------------------------------------

// ApplyDelta applies a git binary delta to base, returning the result.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta ends inside a size field")
			}
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta expects a different base size")
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta ends inside a copy offset")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta ends inside a copy size")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) || written+copySize > len(out) {
				return nil, errors.New("delta copy runs past the end")
			}
			copy(out[written:], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if offset+literal > len(delta) || written+literal > len(out) {
				return nil, errors.New("delta literal runs past the end")
			}
			copy(out[written:], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, errors.New("delta produced the wrong size")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// packfile codec
// ---------------------------------------------------------------------------

var packTypeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var packCodeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// PackResult is the outcome of ReadPack.
type PackResult struct {
	Version         int
	DeclaredCount   int
	Objects         map[string]*Object // oid -> object (deltas resolved)
	Trailer         string
	ComputedTrailer string
	ChecksumOK      bool
	DeltaCount      int
	ByteLength      int
}

// readPackVarint reads a pack object header's size varint.
func readPackVarint(buf []byte, start int) (size int, first byte, next int, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack ends inside an object header")
	}
	b := buf[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, 0, errors.New("pack ends inside an object header")
		}
		b = buf[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, first, offset, nil
}

// readOfsDeltaBase reads an ofs-delta base offset.
func readOfsDeltaBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("pack ends inside an ofs-delta base")
	}
	b := buf[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, errors.New("pack ends inside an ofs-delta base")
		}
		b = buf[offset]
		offset++
		value = ((value + 1) << 7) + int(b&0x7f)
	}
	return value, offset, nil
}

// inflateAt inflates the zlib stream starting at offset (within buf, which
// ends at the pack trailer) and returns the data plus the offset just past
// the stream. The bytes.Reader is left positioned exactly at the stream end
// because flate reads it byte-by-byte.
func inflateAt(buf []byte, offset int) (data []byte, next int, err error) {
	r := bytes.NewReader(buf[offset:])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	data, err = io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	return data, offset + (len(buf) - offset - r.Len()), nil
}

type packEntry struct {
	offset       int
	typeCode     int
	size         int
	data         []byte
	baseOffset   int
	baseOID      string
	resolved     []byte
	resolvedType string
	oid          string
}

// ReadPack parses a packfile, resolving ofs-deltas and ref-deltas, and
// returns the objects it holds keyed by oid.
func ReadPack(pack []byte) (*PackResult, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack is too short")
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(binary.BigEndian.Uint32(pack[4:8]))
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	sum := sha1.Sum(pack[:trailerStart])
	computedTrailer := hex.EncodeToString(sum[:])

	raw := make([]*packEntry, 0, declaredCount)
	byOffset := make(map[int]*packEntry, declaredCount)
	offset := 12
	for i := 0; i < declaredCount; i++ {
		entryOffset := offset
		size, first, next, err := readPackVarint(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		typeCode := int((first >> 4) & 0x07)
		entry := &packEntry{offset: entryOffset, typeCode: typeCode, size: size}
		switch typeCode {
		case 6:
			base, next, err := readOfsDeltaBase(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = next
			entry.baseOffset = entryOffset - base
		case 7:
			if offset+20 > trailerStart {
				return nil, errors.New("pack ends inside a ref-delta base")
			}
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		default:
			if _, ok := packCodeType[typeCode]; !ok {
				return nil, fmt.Errorf("pack entry %d has unknown type code %d", i, typeCode)
			}
		}
		data, next, err := inflateAt(pack[:trailerStart], offset)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		entry.data = data
		offset = next
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, errors.New("pack has stray bytes before its trailer")
	}

	objects := make(map[string]*Object)
	resolvedByOID := make(map[string]*packEntry)
	resolving := make(map[int]bool)
	deltaCount := 0
	var resolve func(e *packEntry) (typ string, data []byte, oid string, err error)
	resolve = func(e *packEntry) (string, []byte, string, error) {
		if e.resolved != nil {
			return e.resolvedType, e.resolved, e.oid, nil
		}
		if resolving[e.offset] {
			return "", nil, "", errors.New("delta cycle in pack")
		}
		resolving[e.offset] = true
		defer delete(resolving, e.offset)
		var typ string
		var data []byte
		switch e.typeCode {
		case 6:
			baseEntry, ok := byOffset[e.baseOffset]
			if !ok {
				return "", nil, "", fmt.Errorf("ofs-delta base at offset %d is not in the pack", e.baseOffset)
			}
			baseType, baseData, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = baseType
			data, err = ApplyDelta(baseData, e.data)
			if err != nil {
				return "", nil, "", err
			}
		case 7:
			baseEntry, ok := resolvedByOID[e.baseOID]
			if !ok {
				return "", nil, "", fmt.Errorf("ref-delta base %s is not in the pack", e.baseOID)
			}
			baseType, baseData, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = baseType
			data, err = ApplyDelta(baseData, e.data)
			if err != nil {
				return "", nil, "", err
			}
		default:
			typ = packCodeType[e.typeCode]
			data = e.data
		}
		oid := HashObject(typ, data)
		e.resolved = data
		e.resolvedType = typ
		e.oid = oid
		resolvedByOID[oid] = e
		return typ, data, oid, nil
	}
	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if entry.typeCode == 6 || entry.typeCode == 7 {
			deltaCount++
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = &Object{Type: typ, Data: data}
		}
	}
	return &PackResult{
		Version:         version,
		DeclaredCount:   declaredCount,
		Objects:         objects,
		Trailer:         trailer,
		ComputedTrailer: computedTrailer,
		ChecksumOK:      trailer == computedTrailer,
		DeltaCount:      deltaCount,
		ByteLength:      len(pack),
	}, nil
}

// writePackObjectHeader encodes a pack object header (type + size varint).
func writePackObjectHeader(typ string, size int) []byte {
	code := packTypeCode[typ]
	var out []byte
	first := byte((code << 4) | (size & 0x0f))
	size >>= 4
	for size > 0 {
		out = append(out, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, first)
	return out
}

// WritePack emits a packfile (version 2, plain objects, no deltas) holding
// the given objects, with the SHA-1 trailer.
func WritePack(objects []*Object) ([]byte, error) {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	buf.Write(header)
	for _, obj := range objects {
		if _, ok := packTypeCode[obj.Type]; !ok {
			return nil, fmt.Errorf("cannot pack object of unknown type %q", obj.Type)
		}
		buf.Write(writePackObjectHeader(obj.Type, len(obj.Data)))
		w := zlib.NewWriter(&buf)
		if _, err := w.Write(obj.Data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// pkt-lines
// ---------------------------------------------------------------------------

// PKT_LINE_MAX is the largest legal pkt-line length (including its header).
const PKT_LINE_MAX = 65520

// EncodePktLine frames payload as one pkt-line.
func EncodePktLine(payload []byte) ([]byte, error) {
	if len(payload) > PKT_LINE_MAX-4 {
		return nil, errors.New("pkt-line payload too long")
	}
	out := make([]byte, 0, len(payload)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(payload)+4))...)
	out = append(out, payload...)
	return out, nil
}

// PktFlush is the 0000 flush packet.
func PktFlush() []byte { return []byte("0000") }

// PktDelim is the 0001 delimiter packet.
func PktDelim() []byte { return []byte("0001") }

// PktPacket is one decoded pkt-line. Kind is "data", "flush", "delim" or
// "response-end"; Payload is set for data packets.
type PktPacket struct {
	Kind    string
	Payload []byte
}

// DecodePktLines decodes a whole buffer into packets.
func DecodePktLines(data []byte) ([]PktPacket, error) {
	var packets []PktPacket
	offset := 0
	isHex4 := func(s string) bool {
		if len(s) != 4 {
			return false
		}
		for _, c := range s {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
		return true
	}
	for offset < len(data) {
		if offset+4 > len(data) {
			return nil, errors.New("truncated pkt-line")
		}
		head := string(data[offset : offset+4])
		if !isHex4(head) {
			return nil, fmt.Errorf("invalid pkt-line length %q", head)
		}
		var length64 int64
		length64, err := strconv.ParseInt(head, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid pkt-line length %q", head)
		}
		length := int(length64)
		switch length {
		case 0:
			packets = append(packets, PktPacket{Kind: "flush"})
			offset += 4
			continue
		case 1:
			packets = append(packets, PktPacket{Kind: "delim"})
			offset += 4
			continue
		case 2:
			packets = append(packets, PktPacket{Kind: "response-end"})
			offset += 4
			continue
		}
		if length < 4 || length > PKT_LINE_MAX {
			return nil, errors.New("invalid pkt-line length")
		}
		if offset+length > len(data) {
			return nil, errors.New("truncated pkt-line")
		}
		packets = append(packets, PktPacket{Kind: "data", Payload: data[offset+4 : offset+length]})
		offset += length
	}
	return packets, nil
}

// DemuxSideband splits sideband-64k framed data into its pack bytes (band 1)
// and progress text (band 2). A band-3 (error) chunk is an error.
func DemuxSideband(chunks [][]byte) (pack []byte, progress string, err error) {
	var packChunks [][]byte
	var prog strings.Builder
	for _, chunk := range chunks {
		if len(chunk) < 1 {
			continue
		}
		switch chunk[0] {
		case 1:
			packChunks = append(packChunks, chunk[1:])
		case 2:
			prog.Write(chunk[1:])
		case 3:
			return nil, "", fmt.Errorf("sideband error: %s", strings.TrimSpace(string(chunk[1:])))
		}
	}
	return bytes.Join(packChunks, nil), prog.String(), nil
}

// SidebandPack frames a pack into sideband-64k pkt-lines (band 1), each
// at most PKT_LINE_MAX bytes.
func SidebandPack(pack []byte) []byte {
	maxChunk := PKT_LINE_MAX - 4 - 1
	var parts [][]byte
	for offset := 0; offset < len(pack); offset += maxChunk {
		end := offset + maxChunk
		if end > len(pack) {
			end = len(pack)
		}
		framed := append([]byte{1}, pack[offset:end]...)
		line, _ := EncodePktLine(framed)
		parts = append(parts, line)
	}
	if len(pack) == 0 {
		line, _ := EncodePktLine([]byte{1})
		parts = append(parts, line)
	}
	return bytes.Join(parts, nil)
}
