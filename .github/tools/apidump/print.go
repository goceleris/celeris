package main

import (
	"crypto/sha256"
	"fmt"
	"go/constant"
	"go/types"
	"math"
	"sort"
	"strconv"
	"strings"
)

// printer renders the exported API of one type-checked package as lines.
//
// Line forms, after cmd/api's (Go's own api/ files) without the "pkg P, "
// prefix, since each file holds one package:
//
//	const Name Type = value
//	var Name Type
//	func Name[$0 C](Params) Results
//	type Name[$0 C] struct                     plus one "type Name struct, Field Type" per exported field,
//	                                           "..., embedded Type" per embedded exported type and
//	                                           "..., promoted Field Type" per exported field reached
//	                                           through an embedded type that has no golden file
//	type Name interface                        plus one "type Name interface, Method(Params) Results" per
//	                                           method of its method set, "..., unexported methods" and
//	                                           "..., embedded T" for a type-set element
//	type Name Underlying                       for any other defined type
//	type Name = Target                         an alias
//	method (Recv) Name(Params) Results         the method set of T and *T, promoted methods included
//	exposed type|method ...                    the same, for a type the API reaches that has no file
//
// Parameter names are left out (renaming one is not an API change), type
// parameters are written $0, $1, ... by position, the empty interface is
// written any, and a type from another package is qualified by its full
// import path.
type printer struct {
	pkg      *types.Package
	rootPath string
	lines    map[string]bool
	exposed  map[*types.TypeName]bool
	queue    []*types.TypeName
}

// extract returns the API lines of a package.
func extract(pkg *types.Package, rootPath string) []string {
	p := &printer{pkg: pkg, rootPath: rootPath, lines: map[string]bool{}, exposed: map[*types.TypeName]bool{}}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		switch o := obj.(type) {
		case *types.Const:
			p.emit("const " + name + " " + p.typ(o.Type()) + " = " + constValue(o.Val()))
		case *types.Var:
			p.emit("var " + name + " " + p.typ(o.Type()))
		case *types.Func:
			sig := o.Type().(*types.Signature)
			p.emit("func " + name + p.tparams(sig.TypeParams()) + p.sig(sig))
		case *types.TypeName:
			p.typeName(o, false)
		}
	}
	for len(p.queue) > 0 {
		obj := p.queue[0]
		p.queue = p.queue[1:]
		p.typeName(obj, true)
	}
	out := make([]string, 0, len(p.lines))
	for l := range p.lines {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func (p *printer) emit(line string) { p.lines[line] = true }

// typeName emits a type declaration and everything that hangs off it.
func (p *printer) typeName(obj *types.TypeName, exposed bool) {
	name, prefix := obj.Name(), ""
	if exposed {
		name, prefix = p.qualifier(obj)+obj.Name(), "exposed "
	}
	switch t := obj.Type().(type) {
	case *types.Alias:
		p.emit(prefix + "type " + name + p.tparams(t.TypeParams()) + " = " + p.typ(t.Rhs()))
	case *types.Named:
		head := prefix + "type " + name + p.tparams(t.TypeParams())
		switch u := t.Underlying().(type) {
		case *types.Struct:
			p.emit(head + " struct")
			p.fields(head+" struct, ", t, u)
		case *types.Interface:
			p.emit(head + " interface")
			p.interfaceLines(head+" interface, ", u)
			return // an interface's methods are listed above, not as a method set
		default:
			p.emit(head + " " + p.typ(u))
		}
		recv := name
		if tps := t.TypeParams(); tps.Len() > 0 {
			args := make([]string, tps.Len())
			for i := range args {
				args[i] = "$" + strconv.Itoa(i)
			}
			recv += "[" + strings.Join(args, ", ") + "]"
		}
		p.methods(prefix, recv, t)
	}
}

// fields lists a struct's exported fields, its embedded exported types, and
// the exported fields promoted from embedded types that have no golden file
// of their own (unexported ones, or ones from internal packages).
func (p *printer) fields(head string, outer *types.Named, st *types.Struct) {
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Embedded() {
			if f.Exported() {
				p.emit(head + "embedded " + p.typ(f.Type()))
			}
			if p.hidden(f.Type()) {
				p.promoted(head, outer, f.Type(), map[types.Type]bool{})
			}
			continue
		}
		if f.Exported() {
			p.emit(head + f.Name() + " " + p.typ(f.Type()))
		}
	}
}

func (p *printer) promoted(head string, outer *types.Named, embedded types.Type, seen map[types.Type]bool) {
	t := deref(embedded)
	if seen[t] {
		return
	}
	seen[t] = true
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return
	}
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Exported() {
			// Only a field the selector outer.Name actually reaches: not one
			// shadowed by a shallower field, nor an ambiguous one.
			if obj, _, _ := types.LookupFieldOrMethod(outer, false, p.pkg, f.Name()); obj == f {
				p.emit(head + "promoted " + f.Name() + " " + p.typ(f.Type()))
			}
		}
		if f.Embedded() && p.hidden(f.Type()) {
			p.promoted(head, outer, f.Type(), seen)
		}
	}
}

// interfaceLines lists every method of an interface's method set, embedded
// interfaces expanded (Go's compatibility rules keep a published interface
// from gaining methods, so this holds still across Go and dependency
// releases), and its type-set elements.
func (p *printer) interfaceLines(head string, it *types.Interface) {
	unexported := false
	for i := 0; i < it.NumMethods(); i++ {
		m := it.Method(i)
		if !m.Exported() {
			unexported = true
			continue
		}
		p.emit(head + m.Name() + p.sig(m.Type().(*types.Signature)))
	}
	if unexported {
		p.emit(head + "unexported methods")
	}
	for i := 0; i < it.NumEmbeddeds(); i++ {
		e := it.EmbeddedType(i)
		if ei, ok := e.Underlying().(*types.Interface); ok && ei.IsMethodSet() {
			continue // its methods are in the method set above
		}
		p.emit(head + "embedded " + p.typ(e))
	}
}

// methods lists the exported methods of T's and *T's method sets, with the
// receiver T for a method of T's set and *T for one only *T has. A method
// promoted from an embedded type of another module or the standard library
// is left out: the "embedded" line names that type, and its methods are its
// own module's API (a Go release that adds a method to time.Time must not
// change these files).
func (p *printer) methods(prefix, recv string, t *types.Named) {
	all := types.NewMethodSet(types.NewPointer(t))
	val := types.NewMethodSet(t)
	for i := 0; i < all.Len(); i++ {
		m := all.At(i).Obj()
		if !m.Exported() || !p.ours(m.Pkg()) {
			continue
		}
		r := recv
		if val.Lookup(m.Pkg(), m.Name()) == nil {
			r = "*" + recv
		}
		p.emit(prefix + "method (" + r + ") " + m.Name() + p.sig(m.Type().(*types.Signature)))
	}
}

// hidden reports whether a type (through pointers and aliases) is a named
// type with no golden file of its own: its API is listed where it is used.
func (p *printer) hidden(t types.Type) bool {
	if n, ok := types.Unalias(deref(t)).(*types.Named); ok {
		return p.undocumented(n.Origin().Obj())
	}
	return false
}

// undocumented reports whether a package-level type has no golden file: an
// unexported type of this package, or any type of a package of this
// repository that gets no file (internal ones). Types of other modules and
// the standard library are documented elsewhere.
func (p *printer) undocumented(obj *types.TypeName) bool {
	pkg := obj.Pkg()
	if pkg == nil || obj.Parent() != pkg.Scope() {
		return false
	}
	if pkg == p.pkg {
		return !obj.Exported()
	}
	if !p.ours(pkg) {
		return false
	}
	return !obj.Exported() || excludedPath(strings.TrimPrefix(strings.TrimPrefix(pkg.Path(), p.rootPath), "/"))
}

// ours reports whether a package belongs to this repository's modules
// (internal ones included); nil is the universe (error's Error method).
func (p *printer) ours(pkg *types.Package) bool {
	if pkg == nil {
		return false
	}
	path := pkg.Path()
	return path == p.rootPath || strings.HasPrefix(path, p.rootPath+"/")
}

// note queues an undocumented type the API reaches, to be listed as exposed.
func (p *printer) note(obj *types.TypeName) {
	if !p.exposed[obj] && p.undocumented(obj) {
		p.exposed[obj] = true
		p.queue = append(p.queue, obj)
	}
}

func (p *printer) qualifier(obj types.Object) string {
	if obj.Pkg() == nil || obj.Pkg() == p.pkg {
		return ""
	}
	return obj.Pkg().Path() + "."
}

func (p *printer) tparams(tps *types.TypeParamList) string {
	if tps.Len() == 0 {
		return ""
	}
	parts := make([]string, tps.Len())
	for i := range parts {
		parts[i] = "$" + strconv.Itoa(i) + " " + p.typ(tps.At(i).Constraint())
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (p *printer) sig(s *types.Signature) string {
	var b strings.Builder
	p.writeSig(&b, s)
	return b.String()
}

func (p *printer) typ(t types.Type) string {
	var b strings.Builder
	p.write(&b, t)
	return b.String()
}

func (p *printer) writeSig(b *strings.Builder, s *types.Signature) {
	b.WriteByte('(')
	params := s.Params()
	for i := 0; i < params.Len(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		t := params.At(i).Type()
		if s.Variadic() && i == params.Len()-1 {
			b.WriteString("...")
			if sl, ok := t.(*types.Slice); ok {
				t = sl.Elem()
			}
		}
		p.write(b, t)
	}
	b.WriteByte(')')
	res := s.Results()
	switch res.Len() {
	case 0:
	case 1:
		b.WriteByte(' ')
		p.write(b, res.At(0).Type())
	default:
		b.WriteString(" (")
		for i := 0; i < res.Len(); i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			p.write(b, res.At(i).Type())
		}
		b.WriteByte(')')
	}
}

func (p *printer) typeArgs(b *strings.Builder, args *types.TypeList) {
	if args.Len() == 0 {
		return
	}
	b.WriteByte('[')
	for i := 0; i < args.Len(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		p.write(b, args.At(i))
	}
	b.WriteByte(']')
}

func (p *printer) write(b *strings.Builder, t types.Type) {
	switch t := t.(type) {
	case *types.Basic:
		if t.Kind() == types.UnsafePointer {
			b.WriteString("unsafe.Pointer")
			return
		}
		b.WriteString(t.Name())
	case *types.Alias:
		obj := t.Obj()
		if obj.Pkg() == nil && obj.Name() == "any" {
			b.WriteString("any")
			return
		}
		p.note(t.Origin().Obj())
		b.WriteString(p.qualifier(obj) + obj.Name())
		p.typeArgs(b, t.TypeArgs())
	case *types.Named:
		obj := t.Obj()
		p.note(t.Origin().Obj())
		b.WriteString(p.qualifier(obj) + obj.Name())
		p.typeArgs(b, t.TypeArgs())
	case *types.TypeParam:
		b.WriteString("$" + strconv.Itoa(t.Index()))
	case *types.Pointer:
		b.WriteByte('*')
		p.write(b, t.Elem())
	case *types.Slice:
		b.WriteString("[]")
		p.write(b, t.Elem())
	case *types.Array:
		fmt.Fprintf(b, "[%d]", t.Len())
		p.write(b, t.Elem())
	case *types.Map:
		b.WriteString("map[")
		p.write(b, t.Key())
		b.WriteByte(']')
		p.write(b, t.Elem())
	case *types.Chan:
		switch t.Dir() {
		case types.SendOnly:
			b.WriteString("chan<- ")
			p.write(b, t.Elem())
		case types.RecvOnly:
			b.WriteString("<-chan ")
			p.write(b, t.Elem())
		default:
			b.WriteString("chan ")
			if c, ok := t.Elem().(*types.Chan); ok && c.Dir() == types.RecvOnly {
				b.WriteByte('(')
				p.write(b, c)
				b.WriteByte(')')
			} else {
				p.write(b, t.Elem())
			}
		}
	case *types.Signature:
		b.WriteString("func")
		p.writeSig(b, t)
	case *types.Struct:
		b.WriteString("struct{")
		for i := 0; i < t.NumFields(); i++ {
			if i > 0 {
				b.WriteString("; ")
			}
			f := t.Field(i)
			if !f.Embedded() {
				b.WriteString(f.Name() + " ")
			}
			p.write(b, f.Type())
			if tag := t.Tag(i); tag != "" {
				b.WriteString(" " + strconv.Quote(tag))
			}
		}
		b.WriteByte('}')
	case *types.Interface:
		p.writeInterface(b, t)
	case *types.Union:
		for i := 0; i < t.Len(); i++ {
			if i > 0 {
				b.WriteString(" | ")
			}
			term := t.Term(i)
			if term.Tilde() {
				b.WriteByte('~')
			}
			p.write(b, term.Type())
		}
	default:
		panic(fmt.Sprintf("apidump: unexpected type %T (%v)", t, t))
	}
}

// writeInterface writes an interface literal: any for the empty one, and a
// lone type-set element without its interface{} wrapper, so [T ~int] and
// [T interface{ ~int }] read the same.
func (p *printer) writeInterface(b *strings.Builder, t *types.Interface) {
	nm, ne := t.NumExplicitMethods(), t.NumEmbeddeds()
	if nm == 0 && ne == 0 {
		b.WriteString("any")
		return
	}
	if nm == 0 && ne == 1 {
		if _, isIface := t.EmbeddedType(0).Underlying().(*types.Interface); !isIface {
			p.write(b, t.EmbeddedType(0))
			return
		}
	}
	b.WriteString("interface{")
	for i := 0; i < nm; i++ {
		if i > 0 {
			b.WriteString("; ")
		}
		m := t.ExplicitMethod(i)
		b.WriteString(m.Name())
		p.writeSig(b, m.Type().(*types.Signature))
	}
	for i := 0; i < ne; i++ {
		if nm > 0 || i > 0 {
			b.WriteString("; ")
		}
		p.write(b, t.EmbeddedType(i))
	}
	b.WriteByte('}')
}

func deref(t types.Type) types.Type {
	if pt, ok := types.Unalias(t).(*types.Pointer); ok {
		return pt.Elem()
	}
	return t
}

// constValue writes a constant's value exactly. A string longer than 100
// bytes is written as its length and SHA-256, so a change still shows.
func constValue(v constant.Value) string {
	switch v.Kind() {
	case constant.String:
		s := constant.StringVal(v)
		if len(s) > 100 {
			return fmt.Sprintf("<%d-byte string, sha256 %x>", len(s), sha256.Sum256([]byte(s)))
		}
		return strconv.Quote(s)
	case constant.Float:
		if f, _ := constant.Float64Val(v); !math.IsInf(f, 0) {
			return strconv.FormatFloat(f, 'g', -1, 64)
		}
	}
	return v.ExactString()
}

// render writes one golden file.
func render(a *pkgAPI) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", a.path)
	b.WriteString("# The exported API, generated by .github/tools/apidump: do not edit, run `mage api`.\n")
	built := "every configuration (api/README.md)"
	if a.present != allConfigs() {
		built = describe(a.present)
	}
	fmt.Fprintf(&b, "# Built for: %s.\n", built)
	lines := make([]string, 0, len(a.facts))
	for l, set := range a.facts {
		if set != a.present {
			l += " // only: " + describe(set)
		}
		lines = append(lines, l)
	}
	// The package's own API first, then the exposed types it reaches.
	sort.Slice(lines, func(i, j int) bool {
		ei, ej := strings.HasPrefix(lines[i], "exposed "), strings.HasPrefix(lines[j], "exposed ")
		if ei != ej {
			return ej
		}
		return lines[i] < lines[j]
	})
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func allConfigs() uint32 { return uint32(1)<<(len(platforms)*len(tagSets)) - 1 }

// describe names a set of configurations, e.g. "linux/amd64 linux/arm64",
// "(-tags=validation)" or "linux/amd64 (no tags, -tags=celeris_closeprobe);
// windows/amd64". Platforms that hold the set with the same tag sets form a
// group; a group lists its tag sets in parentheses unless it has all of
// them, and a group of every platform names only its tag sets.
func describe(set uint32) string {
	full := uint32(1)<<len(tagSets) - 1
	type group struct {
		tags      uint32
		platforms []string
	}
	var groups []*group
	for i, pl := range platforms {
		tags := set >> (i * len(tagSets)) & full
		if tags == 0 {
			continue
		}
		var g *group
		for _, h := range groups {
			if h.tags == tags {
				g = h
			}
		}
		if g == nil {
			g = &group{tags: tags}
			groups = append(groups, g)
		}
		g.platforms = append(g.platforms, pl)
	}
	parts := make([]string, len(groups))
	for i, g := range groups {
		s := strings.Join(g.platforms, " ")
		if len(g.platforms) == len(platforms) {
			s = ""
		}
		if g.tags != full {
			var ts []string
			for j, t := range tagSets {
				if g.tags&(1<<j) != 0 {
					if t == "" {
						ts = append(ts, "no tags")
					} else {
						ts = append(ts, "-tags="+t)
					}
				}
			}
			if s != "" {
				s += " "
			}
			s += "(" + strings.Join(ts, ", ") + ")"
		}
		parts[i] = s
	}
	return strings.Join(parts, "; ")
}
