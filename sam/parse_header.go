// Copyright ©2012 The bíogo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sam

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	errBadHeader = errors.New("sam: malformed header line")
	errDupTag    = errors.New("sam: duplicate field")
)

var bamMagic = [4]byte{'B', 'A', 'M', 0x1}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface.
func (bh *Header) UnmarshalBinary(b []byte) error {
	return bh.DecodeBinary(bytes.NewReader(b))
}

// DecodeBinary unmarshals a Header from the given io.Reader. The byte
// stream must be in the format described in the SAM specification,
// section 4.2.
func (bh *Header) DecodeBinary(r io.Reader) error {
	var (
		lText, nRef int32
		err         error
	)
	var magic [4]byte
	err = binary.Read(r, binary.LittleEndian, &magic)
	if err != nil {
		return err
	}
	if magic != bamMagic {
		return errors.New("sam: magic number mismatch")
	}
	err = binary.Read(r, binary.LittleEndian, &lText)
	if err != nil {
		return err
	}
	if lText < 0 {
		return errors.New("sam: invalid text length")
	}
	text := make([]byte, lText)
	n, err := r.Read(text)
	if err != nil {
		return err
	}
	if n != int(lText) {
		return errors.New("sam: truncated header")
	}
	err = bh.UnmarshalText(text)
	if err != nil {
		return err
	}
	err = binary.Read(r, binary.LittleEndian, &nRef)
	if err != nil {
		return err
	}
	if nRef < 0 {
		return errors.New("sam: invalid reference count field")
	}
	refs, err := readRefRecords(r, nRef)
	if err != nil {
		return err
	}
	// fmt.Println(refs)
	for _, r := range refs {
		err = bh.AddReference(r)
		// fmt.Println("Reference", r, r.id)
		// if err == errUsedReference {
		// 	fmt.Println("Error thrown here", r, r.id)
		// }
		if err != nil {
			return err
		}
	}
	return nil
}

func readRefRecords(r io.Reader, n int32) ([]*Reference, error) {
	// bootstrapSize is the maximum number of
	// reference records to pre-allocate.
	const bootstrapSize = 1000

	rr := make([]*Reference, 0, min(n, bootstrapSize))
	var (
		lName int32
		err   error
	)
	for i := 0; i < int(n); i++ {
		rr = append(rr, &Reference{id: int32(i)})
		err = binary.Read(r, binary.LittleEndian, &lName)
		if err != nil {
			return nil, err
		}
		if lName < 1 {
			return nil, errors.New("sam: invalid name length")
		}
		name := make([]byte, lName)
		n, err := r.Read(name)
		if err != nil {
			return nil, err
		}
		if n != int(lName) || name[n-1] != 0 {
			return nil, errors.New("sam: truncated reference name")
		}
		rr[i].name = string(name[:n-1])
		err = binary.Read(r, binary.LittleEndian, &rr[i].lRef)
		if err != nil {
			return nil, err
		}
	}
	return rr, nil
}

func min(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

// UnmarshalText implements the encoding.TextUnmarshaler interface.
func (bh *Header) UnmarshalText(text []byte) error {
	if bh.seenRefs == nil {
		bh.seenRefs = set{}
	}
	if bh.seenGroups == nil {
		bh.seenGroups = set{}
	}
	if bh.seenProgs == nil {
		bh.seenProgs = set{}
	}
	var t Tag
	for i, l := range bytes.Split(text, []byte{'\n'}) {
		if len(l) > 0 && l[len(l)-1] == '\r' {
			l = l[:len(l)-1]
		}
		if len(l) == 0 {
			continue
		}
		if l[0] != '@' || len(l) < 3 {
			return errBadHeader
		}
		copy(t[:], l[1:3])
		var err error
		switch t {
		case headerTag:
			err = headerLine(l, bh)
		case refDictTag:
			err = referenceLine(l, bh)
		case readGroupTag:
			err = readGroupLine(l, bh)
		case programTag:
			err = programLine(l, bh)
		case commentTag:
			err = commentLine(l, bh)
		default:
			return errBadHeader
		}
		if err != nil {
			return fmt.Errorf("%v: line %d: %q", err, i+1, l)
		}
	}

	return nil
}

func headerLine(l []byte, bh *Header) error {
	fields := bytes.Split(l, []byte{'\t'})
	if len(fields) < 2 {
		return errBadHeader
	}

	var t Tag
	for _, f := range fields[1:] {
		if f[2] != ':' {
			return errBadHeader
		}
		copy(t[:], f[:2])
		fs := string(f[3:])
		switch t {
		case versionTag:
			if bh.Version != "" {
				return errBadHeader
			}
			bh.Version = fs
		case sortOrderTag:
			if bh.SortOrder != UnknownOrder {
				return errBadHeader
			}
			bh.SortOrder = sortOrderMap[fs]
		case groupOrderTag:
			if bh.GroupOrder != GroupUnspecified {
				return errBadHeader
			}
			bh.GroupOrder = groupOrderMap[fs]
		default:
			bh.otherTags = append(bh.otherTags, tagPair{tag: t, value: fs})
		}
	}

	if bh.Version == "" {
		return errBadHeader
	}

	return nil
}

func referenceLine(l []byte, bh *Header) error {
	fields := bytes.Split(l, []byte{'\t'})
	if len(fields) < 3 {
		return errBadHeader
	}

	var (
		t        Tag
		rf       = &Reference{}
		seen     = map[Tag]struct{}{}
		nok, lok bool
		dupID    int32
		dup      bool
	)

	for _, f := range fields[1:] {
		if f[2] != ':' {
			return errBadHeader
		}
		copy(t[:], f[:2])
		if _, ok := seen[t]; ok {
			return errDupTag
		}
		seen[t] = struct{}{}
		fs := string(f[3:])
		switch t {
		case refNameTag:
			dupID, dup = bh.seenRefs[fs]
			rf.name = fs
			nok = true
		case refLengthTag:
			l, err := strconv.Atoi(fs)
			if err != nil {
				return errBadHeader
			}
			if !validLen(l) {
				return errBadLen
			}
			rf.lRef = int32(l)
			lok = true
		case assemblyIDTag:
			rf.assemID = fs
		case md5Tag:
			hb := [16]byte{}
			n, err := hex.Decode(hb[:], f[3:])
			if err != nil {
				return err
			}
			if n != 16 {
				return errBadHeader
			}
			rf.md5 = string(hb[:])
		case speciesTag:
			rf.species = fs
		case uriTag:
			var err error
			rf.uri, err = url.Parse(fs)
			if err != nil {
				return err
			}
			if rf.uri.Scheme != "http" && rf.uri.Scheme != "ftp" {
				rf.uri.Scheme = "file"
			}
		default:
			rf.otherTags = append(rf.otherTags, tagPair{tag: t, value: fs})
		}
	}

	if dup {
		if er := bh.refs[dupID]; equalRefs(er, rf) {
			return nil
		} else if !equalRefs(er, &Reference{id: er.id, name: er.name, lRef: er.lRef}) {
			return errDupReference
		}
		old := bh.refs[dupID]
		old.owner = nil
		old.id = -1
		bh.refs[dupID] = rf
		rf.owner = bh
		return nil
	}
	if !nok || !lok {
		return errBadHeader
	}
	id := int32(len(bh.refs))
	rf.owner = bh
	rf.id = id
	bh.seenRefs[rf.name] = id
	bh.refs = append(bh.refs, rf)

	return nil
}

// http://en.wikipedia.org/wiki/ISO_8601
//
// Date: 2014-08-13
// Time: 2014-08-13T16:02:01Z
//     : 2014-08-13T16:02:01
//     : 2014-08-13T16:02:01+00:00
//     : 2014-08-13T16:02:01.000+00:00
//
const (
	// This is the ISO8601 format used for output.
	iso8601TimeDateN = "2006-01-02T15:04:05-0700"

	// This is the set of ISO8601 formats we accept.
	// The input values are first converted to a
	// basic ISO8601 form by removing all ':'
	// characters. We cannot do the same thing with
	// '-' since this has two meanings in ISO8601,
	// a separator and a negative time zone offset.
	iso8601DateB          = "20060102"
	iso8601DateE          = "2006-01-02"
	iso8601TimeDateB      = "20060102T150405"
	iso8601TimeDateE      = "2006-01-02T150405"
	iso8601TimeDateZB     = "20060102T150405Z"
	iso8601TimeDateZE     = "2006-01-02T150405Z"
	iso8601TimeDateNB     = "20060102T150405-0700"
	iso8601TimeDateNE     = "2006-01-02T150405-0700"
	iso8601TimeThouDateZB = "20060102T150405.999Z"
	iso8601TimeThouDateZE = "2006-01-02T150405.999Z"
	iso8601TimeThouDateNB = "20060102T150405.999-0700"
	iso8601TimeThouDateNE = "2006-01-02T150405.999-0700"
)

var iso8601 = []struct {
	isLocal bool
	format  string
}{
	{isLocal: true, format: iso8601DateB},
	{isLocal: true, format: iso8601DateE},
	{isLocal: false, format: iso8601TimeDateZB},
	{isLocal: false, format: iso8601TimeDateZE},
	{isLocal: false, format: iso8601TimeDateNB},
	{isLocal: false, format: iso8601TimeDateNE},
	{isLocal: false, format: iso8601TimeThouDateZB},
	{isLocal: false, format: iso8601TimeThouDateZE},
	{isLocal: false, format: iso8601TimeThouDateNB},
	{isLocal: false, format: iso8601TimeThouDateNE},
	{isLocal: true, format: iso8601TimeDateB},
	{isLocal: true, format: iso8601TimeDateE},
}

func parseISO8601(value string) (time.Time, error) {
	value = strings.Replace(value, ":", "", -1)
	var err error
	for _, format := range iso8601 {
		loc := time.UTC
		if format.isLocal {
			loc = time.Local
		}
		var t time.Time
		t, err = time.ParseInLocation(format.format, value, loc)
		if err == nil {
			return t, nil
		}
	}
	return time.Time{}, err
}

func readGroupLine(l []byte, bh *Header) error {
	fields := bytes.Split(l, []byte{'\t'})
	if len(fields) < 2 {
		return errBadHeader
	}

	var (
		t    Tag
		rg   = &ReadGroup{}
		seen = map[Tag]struct{}{}
		idok bool
	)

	for _, f := range fields[1:] {
		if f[2] != ':' {
			return errBadHeader
		}
		copy(t[:], f[:2])
		if _, ok := seen[t]; ok {
			return errDupTag
		}
		seen[t] = struct{}{}
		fs := string(f[3:])
		switch t {
		case idTag:
			if _, ok := bh.seenGroups[fs]; ok {
				return errDupReadGroup
			}
			rg.name = fs
			idok = true
		case centerTag:
			rg.center = fs
		case descriptionTag:
			rg.description = fs
		case dateTag:
			var err error
			rg.date, err = parseISO8601(fs)
			if err != nil {
				return err
			}
		case flowOrderTag:
			rg.flowOrder = fs
		case keySequenceTag:
			rg.keySeq = fs
		case libraryTag:
			rg.library = fs
		case programTag:
			rg.program = fs
		case insertSizeTag:
			i, err := strconv.Atoi(fs)
			if err != nil {
				return err
			}
			if !validInt32(i) {
				return errBadLen
			}
			rg.insertSize = i
		case platformTag:
			rg.platform = fs
		case platformUnitTag:
			rg.platformUnit = fs
		case sampleTag:
			rg.sample = fs
		default:
			rg.otherTags = append(rg.otherTags, tagPair{tag: t, value: fs})
		}
	}

	if !idok {
		return errBadHeader
	}
	id := int32(len(bh.rgs))
	rg.owner = bh
	rg.id = id
	bh.seenGroups[rg.name] = id
	bh.rgs = append(bh.rgs, rg)

	return nil
}

func programLine(l []byte, bh *Header) error {
	fields := bytes.Split(l, []byte{'\t'})
	if len(fields) < 2 {
		return errBadHeader
	}

	var (
		t    Tag
		p    = &Program{}
		seen = map[Tag]struct{}{}
		idok bool
	)

	for _, f := range fields[1:] {
		if f[2] != ':' {
			return errBadHeader
		}
		copy(t[:], f[:2])
		if _, ok := seen[t]; ok {
			return errDupTag
		}
		seen[t] = struct{}{}
		fs := string(f[3:])
		switch t {
		case idTag:
			if _, ok := bh.seenProgs[fs]; ok {
				return errDupProgram
			}
			p.uid = fs
			idok = true
		case programNameTag:
			p.name = fs
		case commandLineTag:
			p.command = fs
		case previousProgTag:
			p.previous = fs
		case versionTag:
			p.version = fs
		default:
			p.otherTags = append(p.otherTags, tagPair{tag: t, value: fs})
		}
	}

	if !idok {
		return errBadHeader
	}
	id := int32(len(bh.progs))
	p.owner = bh
	p.id = id
	bh.seenProgs[p.uid] = id
	bh.progs = append(bh.progs, p)

	return nil
}

func commentLine(l []byte, bh *Header) error {
	fields := bytes.Split(l, []byte{'\t'})
	if len(fields) < 2 {
		return errBadHeader
	}
	bh.Comments = append(bh.Comments, string(fields[1]))
	return nil
}
