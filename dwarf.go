package wzprof

import (
	"debug/dwarf"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"sort"

	"github.com/tetratelabs/wazero/api"
)

type pcrange = [2]uint64

type entryatrange struct {
	Range pcrange
	Entry *dwarf.Entry
}

type subprogram struct {
	Entry     *dwarf.Entry
	CU        *dwarf.Entry
	Inlines   []*dwarf.Entry
	Namespace string
}

type subprogramRange struct {
	Range      pcrange
	Subprogram *subprogram
}

type dwarfmapper struct {
	d           *dwarf.Data
	subprograms []subprogramRange
}

func newDwarfmapper(sections []api.CustomSection) (mapper, error) {
	var info, line, ranges, str, abbrev []byte

	for _, section := range sections {
		switch section.Name() {
		case ".debug_info":
			info = section.Data()
		case ".debug_line":
			line = section.Data()
		case ".debug_str":
			str = section.Data()
		case ".debug_abbrev":
			abbrev = section.Data()
		case ".debug_ranges":
			ranges = section.Data()
		}
	}

	if info == nil {
		return nil, fmt.Errorf("dwarf: missing section: .debug_info")
	}
	if line == nil {
		return nil, fmt.Errorf("dwarf: missing section: .debug_line")
	}
	if str == nil {
		return nil, fmt.Errorf("dwarf: missing section: .debug_str")
	}
	if abbrev == nil {
		return nil, fmt.Errorf("dwarf: missing section: .debug_abbrev")
	}
	if ranges == nil {
		return nil, fmt.Errorf("dwarf: missing section: .debug_ranges")
	}

	d, _ := dwarf.New(abbrev, nil, nil, info, line, nil, ranges, str)

	r := d.Reader()

	p := dwarfparser{d: d, r: r}
	subprograms := p.Parse()

	dm := &dwarfmapper{
		d:           d,
		subprograms: subprograms,
	}

	return dm, nil
}

type dwarfparser struct {
	d *dwarf.Data
	r *dwarf.Reader

	subprograms []subprogramRange
}

func (d *dwarfparser) Parse() []subprogramRange {
	for {
		ent, err := d.r.Next()
		if err != nil || ent == nil {
			break
		}
		if ent.Tag == dwarf.TagCompileUnit {
			d.parseCompileUnit(ent, "")
		} else {
			d.r.SkipChildren()
		}
	}
	return d.subprograms
}

func (d *dwarfparser) parseCompileUnit(cu *dwarf.Entry, ns string) {
	// Assumption is that r has just read the top level entry of the CU (or
	// possibly a namespace), that is cu.
	d.parseAny(cu, ns, cu)
}

func (d *dwarfparser) parseAny(cu *dwarf.Entry, ns string, e *dwarf.Entry) {
	// Assumption is that r has just read the top level entry e.

	for e.Children {
		ent, err := d.r.Next()
		if err != nil || ent == nil {
			return
		}

		switch ent.Tag {
		case 0:
			// end of block
			return
		case dwarf.TagSubprogram:
			d.parseSubprogram(cu, ns, ent)
		case dwarf.TagNamespace:
			d.parseNamespace(cu, ns, ent)
		default:
			d.parseAny(cu, ns, ent)
		}
	}
}

func (d *dwarfparser) parseNamespace(cu *dwarf.Entry, ns string, e *dwarf.Entry) {
	// Assumption is that r has just read the top level entry of this
	// namespace, which is e.

	name, ok := e.Val(dwarf.AttrName).(string)
	if ok {
		ns += name + ":" // TODO: string builder.
	}
	d.parseCompileUnit(cu, ns)
}

func (d *dwarfparser) parseSubprogram(cu *dwarf.Entry, ns string, e *dwarf.Entry) {
	// Assumption is r has just read the top entry of the subprogram, which
	// is e.

	var inlines []*dwarf.Entry

	for e.Children {
		ent, err := d.r.Next()
		if err != nil || ent == nil {
			break
		}
		if ent.Tag == 0 {
			break
		}
		if ent.Tag != dwarf.TagInlinedSubroutine {
			d.r.SkipChildren()
			continue
		}
		inlines = append(inlines, ent)
		// Inlines can have children that describe which variables were
		// used during inlining.
		d.r.SkipChildren()
	}

	ranges, err := d.d.Ranges(e)
	if err != nil {
		log.Printf("profiler: dwarf: failed to read ranges: %s\n", err)
		return
	}

	spgm := &subprogram{
		Entry:     e,
		CU:        cu,
		Inlines:   inlines,
		Namespace: ns,
	}

	if len(ranges) == 0 {
		// If there is no range provided by dwarf, attach this
		// subprogram to an artificial empty range unlikely to be used.
		// This is so that we still have a record of the funciton in the
		// subprograms collection, as that's where the name resolution
		// for inline functions searches for the inlined function.
		// Notably, it's likely that a subprogram without range
		// represent a function that has only been inlined. This
		// situation is temporary until we rework thie subprograms data
		// structure.
		ranges = append(ranges, pcrange{math.MaxUint32, math.MaxUint32})
	}

	for _, pcr := range ranges {
		d.subprograms = append(d.subprograms, subprogramRange{
			Range:      pcr,
			Subprogram: spgm,
		})
	}
}

// Lookup returns a list of function locations for a given program
// counter, starting from current function followed by the inlined
// functions, in order of inlining. Result if empty if the pc cannot
// be resolved in the dwarf data.
func (d *dwarfmapper) Lookup(pc uint64) []location {
	// TODO: replace with binary search

	var spgm *subprogram

	for _, sr := range d.subprograms {
		if sr.Range[0] <= pc && pc <= sr.Range[1] {
			spgm = sr.Subprogram
			break
		}
	}

	if spgm == nil {
		return nil
	}

	lr, err := d.d.LineReader(spgm.CU)
	if err != nil || lr == nil {
		log.Printf("profiler: dwarf: failed to read lines: %s\n", err)
		return nil
	}

	// TODO: cache this
	var lines []line
	var le dwarf.LineEntry
	for {
		pos := lr.Tell()
		err = lr.Next(&le)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Printf("profiler: dwarf: failed to iterate on lines: %s\n", err)
			break
		}
		lines = append(lines, line{Pos: pos, Address: le.Address})

	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].Address < lines[j].Address })

	i := sort.Search(len(lines), func(i int) bool { return lines[i].Address >= pc })
	if i == len(lines) {
		// no line information for this pc.
		return nil
	}

	l := lines[i]
	if l.Address != pc {
		// https://github.com/stealthrocket/wazero/blob/867459d7d5ed988a55452d6317ff3cc8451b8ff0/internal/wasmdebug/dwarf.go#L141-L150
		// If the address doesn't match exactly, the previous
		// entry is the one that contains the instruction.
		// That can happen anytime as the DWARF spec allows
		// it, and other tools can handle it in this way
		// conventionally
		// https://github.com/gimli-rs/addr2line/blob/3a2dbaf84551a06a429f26e9c96071bb409b371f/src/lib.rs#L236-L242
		// https://github.com/kateinoigakukun/wasminspect/blob/f29f052f1b03104da9f702508ac0c1bbc3530ae4/crates/debugger/src/dwarf/mod.rs#L453-L459
		if i-1 < 0 {
			return nil
		}
		l = lines[i-1]
	}

	lr.Seek(l.Pos)
	err = lr.Next(&le)
	if err != nil {
		// l.Pos was created from parsing dwarf, should not
		// happen.
		panic("bug")
	}

	human, stable := d.namesForSubprogram(spgm.Entry, spgm)
	locations := make([]location, 0, 1+len(spgm.Inlines))
	locations = append(locations, location{
		File:       le.File.Name,
		Line:       int64(le.Line),
		Column:     int64(le.Column),
		Inlined:    len(spgm.Inlines) > 0,
		PC:         pc,
		HumanName:  human,
		StableName: stable,
	})

	if len(spgm.Inlines) > 0 {
		files := lr.Files()
		for i := len(spgm.Inlines) - 1; i >= 0; i-- {
			// TODO: check pc is in range of inline?
			f := spgm.Inlines[i]
			fileIdx, ok := f.Val(dwarf.AttrCallFile).(int64)
			if !ok || fileIdx >= int64(len(files)) {
				break
			}
			file := files[fileIdx]
			line, _ := f.Val(dwarf.AttrCallLine).(int64)
			col, _ := f.Val(dwarf.AttrCallLine).(int64)
			human, stable := d.namesForSubprogram(f, nil)
			locations = append(locations, location{
				File:       file.Name,
				Line:       line,
				Column:     col,
				Inlined:    i != 0,
				PC:         pc,
				StableName: stable,
				HumanName:  human,
			})
		}
	}

	return locations
}

// line is used to cache line entries for a given compilation unit.
type line struct {
	Pos     dwarf.LineReaderPos
	Address uint64
}

// Returns a human-readable name and the name the most likely to match the one
// used in the wasm module. Walks up the inlining chain.
//
// Subprogram is optional. This function will look for the associated subprogram
// if spgm is nil.
func (d *dwarfmapper) namesForSubprogram(e *dwarf.Entry, spgm *subprogram) (string, string) {
	// If an inlined function, grab the name from the origin.
	var err error
	r := d.d.Reader()
	for {
		ao, ok := e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset)
		if !ok {
			break
		}
		r.Seek(ao)
		e, err = r.Next()
		if err != nil {
			// malformed dwarf
			break
		}
	}

	// TODO: index
	if spgm == nil {
		for _, s := range d.subprograms {
			if s.Subprogram.Entry.Offset == e.Offset {
				spgm = s.Subprogram
				break
			}
		}
	}

	var ns string
	if spgm != nil {
		ns = spgm.Namespace
	} else {

		//		panic("spgm not found")
	}

	name, _ := e.Val(dwarf.AttrName).(string)
	name = ns + name
	stableName, ok := e.Val(dwarf.AttrLinkageName).(string)
	if !ok {
		stableName = name
	}

	return name, stableName
}
