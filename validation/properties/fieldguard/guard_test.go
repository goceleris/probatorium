package fieldguard

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/goceleris/probatorium/validation/properties"
)

// probatorium#395. I-ENG-IOURING read Snapshot.IOUringSQEsSubmitted and
// .IOUringCQEsCompleted, which nothing ever wrote. Two of its three checks
// compared 0 with 0 on every evaluation, could not fail, and were counted as
// evaluations in every run report. Nine predicates went the same way in
// probatorium#297.
//
// A name search cannot catch it: report.ObserverSample and cmd/observer's
// struct both have an FDCount, so `git grep -w FDCount` finds "writers" for a
// properties.Snapshot field nothing feeds. So this guard decides with the type
// checker: a selector counts only when go/types resolves it to one of
// Snapshot's own fields, which holds however the Snapshot was reached.

// snapshotType ties the guard to the real type at compile time: rename or move
// properties.Snapshot and this module stops compiling instead of quietly
// matching nothing.
var snapshotType = reflect.TypeFor[properties.Snapshot]()

// Vacuity floors. An analysis that stopped seeing the code would find no
// unfed field and pass, so these assert it is still looking. On the tree
// probatorium#403 merges it loads 24 packages and sees 96 of Snapshot's 97
// fields read, at 248 sites (97 in validation/properties, 84 in
// validation/checker, 65 in validation, 2 in cmd/validator-checker), and all
// 97 fed, at 112 sites (89 in validation/checker, 21 in validation, 2 in
// cmd/validator-checker). Each floor sits closer to its total than any one
// of those packages contributes, so going blind to one trips it;
// TestGuardControlVacuity checks that it does.
const (
	floorReadSites  = 200
	floorWriteSites = 100
)

// use is one read or one write of a Snapshot field.
type use struct {
	field string
	at    string // file relative to the root module, then ":" and the line
}

// assignment is a write that is a plain `x.F = expr` statement directly in a
// block. The write-side controls splice the source there, so they keep
// working when the projection code moves.
type assignment struct {
	use
	file string // absolute, as go list reported it
	stmt [2]int // byte offsets of the statement, [start, end)
	lhs  [2]int // byte offsets of its left-hand side
	rhs  [2]int // byte offsets of its right-hand side
}

type analysis struct {
	pkgs        int
	fieldTypes  map[string]string // every field the loaded Snapshot declares, and its type
	fieldZeros  map[string]string // the zero value of each field's type, as Go source
	reads       []use
	writes      []use // writes that can feed the field
	zeroWrites  []use // writes of a constant zero, which leave it reading zero
	assignments []assignment
	// structFile and structClose locate Snapshot's closing brace, so the
	// read-side controls can declare a field there.
	structFile  string
	structClose int
}

// moduleRoot is the probatorium root module, three directories up. It is
// resolved through symlinks once, and that one spelling is used both as the
// directory go list runs in and as the prefix of every overlay path.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(here), "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve the root module: %v", err)
	}
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	want := "module " + strings.TrimSuffix(snapshotType.PkgPath(), "/validation/properties")
	for line := range strings.SplitSeq(string(gomod), "\n") {
		if strings.TrimSpace(line) == want {
			return root
		}
	}
	t.Fatalf("%s/go.mod does not declare %q, so the guard would analyse the wrong module", root, want)
	return ""
}

// analyse is load for a test that cannot go on without the analysis.
func analyse(t *testing.T, root string, overlay map[string][]byte) analysis {
	t.Helper()
	a, err := load(root, overlay)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// load type-checks every package of the root module from source and records
// each read and write of a Snapshot field. overlay replaces or adds file
// contents in memory; it is how the controls inject a defect without
// touching the tree.
func load(root string, overlay map[string][]byte) (analysis, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports |
			packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
		Dir: root,
		// The magefiles belong to the module too. Without the tag they are
		// as invisible here as they are to `go test ./...`.
		BuildFlags: []string{"-tags=mage"},
		Overlay:    overlay,
		// Test files stay out. A test that builds Snapshot{X: 1} to exercise
		// a predicate is not a writer, and counting it would pass a field
		// that only tests ever set.
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return analysis{}, fmt.Errorf("packages.Load: %w", err)
	}
	var loadErrs []string
	var props *packages.Package
	for _, p := range pkgs {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
		if p.PkgPath == snapshotType.PkgPath() {
			props = p
		}
	}
	if len(loadErrs) > 0 {
		// A package that does not type-check has holes in its TypesInfo,
		// and a hole is a read this analysis would never see.
		return analysis{}, fmt.Errorf("the root module does not load cleanly, so the analysis "+
			"would be partial:\n  %s", strings.Join(loadErrs[:min(len(loadErrs), 8)], "\n  "))
	}
	if props == nil {
		return analysis{}, fmt.Errorf("./... under %s did not load %s", root, snapshotType.PkgPath())
	}
	tn, ok := props.Types.Scope().Lookup(snapshotType.Name()).(*types.TypeName)
	if !ok {
		return analysis{}, fmt.Errorf("%s declares no type %s", props.PkgPath, snapshotType.Name())
	}
	st, ok := tn.Type().Underlying().(*types.Struct)
	if !ok {
		return analysis{}, fmt.Errorf("%s is not a struct", tn.Type())
	}

	a := analysis{
		pkgs:       len(pkgs),
		fieldTypes: make(map[string]string, st.NumFields()),
		fieldZeros: make(map[string]string, st.NumFields()),
	}
	fields := make(map[*types.Var]bool, st.NumFields())
	for i := range st.NumFields() {
		f := st.Field(i)
		fields[f] = true
		a.fieldTypes[f.Name()] = types.TypeString(f.Type(), nil)
		a.fieldZeros[f.Name()] = zeroLiteral(f.Type())
	}
	for _, f := range props.Syntax {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, s := range gd.Specs {
				if ts := s.(*ast.TypeSpec); ts.Name.Name == snapshotType.Name() {
					if sx, ok := ts.Type.(*ast.StructType); ok {
						pos := props.Fset.Position(sx.Fields.Closing)
						a.structFile, a.structClose = pos.Filename, pos.Offset
					}
				}
			}
		}
	}

	// A composite literal writes Snapshot's fields when its type is
	// Snapshot or anything with Snapshot's struct as its underlying type.
	isSnapshot := func(typ types.Type) bool {
		if typ == nil {
			return false
		}
		if ptr, ok := types.Unalias(typ).(*types.Pointer); ok {
			typ = ptr.Elem()
		}
		return typ.Underlying() == types.Type(st)
	}
	for _, p := range pkgs {
		at := func(n ast.Node) string {
			pos := p.Fset.Position(n.Pos())
			rel, err := filepath.Rel(root, pos.Filename)
			if err != nil {
				rel = pos.Filename
			}
			return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), pos.Line)
		}
		// write records a write of field at n, unless what it writes is the
		// constant zero: `x.F = 0` or Snapshot{F: 0} leaves F reading zero
		// exactly as no write at all does.
		write := func(field string, n ast.Node, value ast.Expr) bool {
			u := use{field, at(n)}
			if value != nil && isZero(p.TypesInfo, value) {
				a.zeroWrites = append(a.zeroWrites, u)
				return false
			}
			a.writes = append(a.writes, u)
			return true
		}
		for _, f := range p.Syntax {
			var stack []ast.Node
			ast.Inspect(f, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				switch e := n.(type) {
				case *ast.SelectorExpr:
					// Selections resolves the selector by the type of what it
					// selects from, so a parameter, an element of
					// Context.History, a struct field or a promoted field
					// all land here alike.
					sel, ok := p.TypesInfo.Selections[e]
					if !ok || sel.Kind() != types.FieldVal {
						break
					}
					if v, ok := sel.Obj().(*types.Var); !ok || !fields[v] {
						break
					}
					written, value, stmt := writeOf(stack, p.TypesInfo)
					if !written {
						a.reads = append(a.reads, use{e.Sel.Name, at(e)})
						break
					}
					if write(e.Sel.Name, e, value) && stmt != nil {
						off := func(pos token.Pos) int { return p.Fset.Position(pos).Offset }
						a.assignments = append(a.assignments, assignment{
							use:  use{e.Sel.Name, at(e)},
							file: p.Fset.Position(stmt.Pos()).Filename,
							stmt: [2]int{off(stmt.Pos()), off(stmt.End())},
							lhs:  [2]int{off(stmt.Lhs[0].Pos()), off(stmt.Lhs[0].End())},
							rhs:  [2]int{off(stmt.Rhs[0].Pos()), off(stmt.Rhs[0].End())},
						})
					}
				case *ast.CompositeLit:
					if !isSnapshot(p.TypesInfo.TypeOf(e)) {
						break
					}
					for i, elt := range e.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok {
								write(k.Name, kv, kv.Value)
							}
						} else if i < st.NumFields() {
							write(st.Field(i).Name(), elt, elt)
						}
					}
				}
				return true
			})
		}
	}
	return a, nil
}

// atomicWriters are the sync/atomic functions that store through the address
// they are given first. Load* only reads through it.
var atomicWriters = []string{"Store", "Add", "Swap", "CompareAndSwap", "And", "Or"}

// writeOf reports whether the field selector on top of stack is written and,
// when it can tell, the expression written. It also returns the statement
// when the write is a single `x.F = expr` directly in a block. `&x.F` is a
// read, except as the address argument of a sync/atomic function that stores
// through it: handing an address to anything else, even atomic.LoadInt64,
// feeds nothing.
func writeOf(stack []ast.Node, info *types.Info) (bool, ast.Expr, *ast.AssignStmt) {
	i := len(stack) - 1
	child := stack[i]
	for i--; i >= 0; i-- {
		if _, ok := stack[i].(*ast.ParenExpr); !ok {
			break
		}
		child = stack[i]
	}
	if i < 0 {
		return false, nil, nil
	}
	switch p := stack[i].(type) {
	case *ast.AssignStmt:
		at := slices.IndexFunc(p.Lhs, func(l ast.Expr) bool { return l == child })
		if at < 0 {
			return false, nil, nil
		}
		var value ast.Expr
		if len(p.Rhs) == len(p.Lhs) {
			value = p.Rhs[at]
		}
		if p.Tok == token.ASSIGN && len(p.Lhs) == 1 && len(p.Rhs) == 1 && i > 0 {
			switch stack[i-1].(type) {
			case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause:
				return true, value, p
			}
		}
		return true, value, nil
	case *ast.IncDecStmt:
		return p.X == child, nil, nil
	case *ast.RangeStmt:
		return p.Tok == token.ASSIGN && (p.Key == child || p.Value == child), nil, nil
	case *ast.UnaryExpr:
		if p.Op != token.AND {
			return false, nil, nil
		}
		arg := ast.Node(p)
		j := i - 1
		for ; j >= 0; j-- {
			if _, ok := stack[j].(*ast.ParenExpr); !ok {
				break
			}
			arg = stack[j]
		}
		if j < 0 {
			return false, nil, nil
		}
		call, ok := stack[j].(*ast.CallExpr)
		if !ok || len(call.Args) == 0 || call.Args[0] != arg {
			return false, nil, nil
		}
		valueArg, ok := atomicWriter(call, info)
		if !ok {
			return false, nil, nil
		}
		var value ast.Expr
		if valueArg < len(call.Args) {
			value = call.Args[valueArg]
		}
		return true, value, nil
	}
	return false, nil, nil
}

// atomicWriter reports whether call is a sync/atomic function that stores
// through its first argument, and which argument carries the value stored.
func atomicWriter(call *ast.CallExpr, info *types.Info) (valueArg int, ok bool) {
	var id *ast.Ident
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return 0, false
	}
	fn, ok := info.Uses[id].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "sync/atomic" {
		return 0, false
	}
	if sig, ok := fn.Type().(*types.Signature); !ok || sig.Recv() != nil {
		return 0, false
	}
	if !slices.ContainsFunc(atomicWriters, func(p string) bool { return strings.HasPrefix(fn.Name(), p) }) {
		return 0, false
	}
	if strings.HasPrefix(fn.Name(), "CompareAndSwap") {
		return 2, true // (addr, old, new)
	}
	return 1, true
}

// isZero reports whether e is a compile-time constant equal to its type's
// zero value.
func isZero(info *types.Info, e ast.Expr) bool {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil {
		return false
	}
	switch v := tv.Value; v.Kind() {
	case constant.Bool:
		return !constant.BoolVal(v)
	case constant.String:
		return constant.StringVal(v) == ""
	case constant.Int, constant.Float, constant.Complex:
		return constant.Sign(v) == 0
	}
	return false
}

// zeroLiteral is the zero value of a basic type as Go source, or "" for any
// other type.
func zeroLiteral(typ types.Type) string {
	b, ok := typ.Underlying().(*types.Basic)
	switch {
	case !ok:
		return ""
	case b.Info()&types.IsBoolean != 0:
		return "false"
	case b.Info()&types.IsString != 0:
		return `""`
	case b.Info()&types.IsNumeric != 0:
		return "0"
	}
	return ""
}

func fieldSet(us []use) map[string]bool {
	m := make(map[string]bool, len(us))
	for _, u := range us {
		m[u.field] = true
	}
	return m
}

// finding is one field the guard reports, and the message it reports it by.
type finding struct {
	field, message string
}

// findings is the guard's verdict on an analysis: every field something
// reads and nothing feeds, sorted by name.
// TestEveryFieldReadFromASnapshotHasAWriter reports exactly these, and the
// controls judge exactly these, so no control can pass on code the guard
// itself does not run.
func (a analysis) findings() []finding {
	fed := fieldSet(a.writes)
	readAt := map[string][]string{}
	for _, r := range a.reads {
		if !fed[r.field] {
			readAt[r.field] = append(readAt[r.field], r.at)
		}
	}
	zeroes := map[string][]string{}
	for _, w := range a.zeroWrites {
		zeroes[w.field] = append(zeroes[w.field], w.at)
	}
	var out []finding
	for _, f := range slices.Sorted(maps.Keys(readAt)) {
		also := ""
		if z := zeroes[f]; len(z) > 0 {
			also = " (it is only ever set to a constant zero, at " + strings.Join(z, ", ") + ")"
		}
		out = append(out, finding{f, fmt.Sprintf("Snapshot.%s is read at %s and written nowhere%s: "+
			"whatever reads it compares zero with zero and cannot fail (probatorium#395). Feed the "+
			"field, or delete it and narrow the predicate's description.",
			f, strings.Join(readAt[f], ", "), also)})
	}
	return out
}

// newlyReported lists the fields the guard reports on a mutated tree and
// not on the base one.
func newlyReported(a, base analysis) []string {
	before := map[string]bool{}
	for _, f := range base.findings() {
		before[f.field] = true
	}
	var out []string
	for _, f := range a.findings() {
		if !before[f.field] {
			out = append(out, f.field)
		}
	}
	return out
}

// TestEveryFieldReadFromASnapshotHasAWriter is the guard probatorium#395 asks
// for: a Snapshot field that something in the module reads and nothing in it
// feeds is a check that cannot fail, reported as coverage in the run report.
// A write of a constant zero does not feed: the field still reads zero.
func TestEveryFieldReadFromASnapshotHasAWriter(t *testing.T) {
	root := moduleRoot(t)
	a := analyse(t, root, nil)
	t.Logf("fieldguard: %d packages; Snapshot declares %d fields; %d fields read at %d sites; "+
		"%d fields fed at %d sites; %d constant-zero writes not counted as feeding",
		a.pkgs, len(a.fieldTypes), len(fieldSet(a.reads)), len(a.reads),
		len(fieldSet(a.writes)), len(a.writes), len(a.zeroWrites))

	if len(a.fieldTypes) != snapshotType.NumField() {
		t.Fatalf("the Snapshot go list loaded declares %d fields and the one this test compiled "+
			"against declares %d: the analysis is not looking at the tree it was built with",
			len(a.fieldTypes), snapshotType.NumField())
	}
	for _, check := range []func(analysis) error{underFloors, missingPositiveControl} {
		if err := check(a); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range a.findings() {
		t.Error(f.message)
	}
}

// underFloors fails an analysis that has stopped seeing the code.
func underFloors(a analysis) error {
	var errs []error
	if len(a.reads) < floorReadSites {
		errs = append(errs, fmt.Errorf("%d read sites, under the floor of %d", len(a.reads), floorReadSites))
	}
	if len(a.writes) < floorWriteSites {
		errs = append(errs, fmt.Errorf("%d write sites, under the floor of %d", len(a.writes), floorWriteSites))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("the analysis is no longer seeing the code, so the guard proves nothing: %w", err)
	}
	return nil
}

// positiveControls are fields read in validation/properties and fed from
// other packages (the checker's /debug/vars poll, the orchestrator's RSS
// sampler), so both halves of the scan and the cross-package field identity
// are live whenever each is found on both sides.
var positiveControls = []string{"IouringSQECorruptions", "RSSBytes", "AcceptedConnTotal"}

// missingPositiveControl fails an analysis that lost a positive control on
// either side.
func missingPositiveControl(a analysis) error {
	read, fed := fieldSet(a.reads), fieldSet(a.writes)
	for _, f := range positiveControls {
		if !read[f] || !fed[f] {
			return fmt.Errorf("positive control %s: read=%v fed=%v; either half of the scan is "+
				"broken, or the field stopped being a good control", f, read[f], fed[f])
		}
	}
	return nil
}

// TestGuardControlVacuity proves the guard fails loudly when it stops
// seeing the code rather than passing on an empty analysis. Each check is
// driven on its own: going blind to the reads, or to the writes, of any one
// of the packages that hold nearly all of them trips that side's floor;
// losing one positive control's writer trips that check while the floors
// still hold; and a tree that does not type-check is refused outright.
func TestGuardControlVacuity(t *testing.T) {
	root := moduleRoot(t)
	base := analyse(t, root, nil)
	if err := errors.Join(underFloors(base), missingPositiveControl(base)); err != nil {
		t.Fatalf("negative control: the real tree must pass: %v", err)
	}
	blindCases := []struct {
		side, dir string
	}{
		{"reads", "validation/properties"},
		{"reads", "validation/checker"},
		{"reads", "validation"},
		{"writes", "validation/checker"},
		{"writes", "validation"},
	}
	for _, c := range blindCases {
		t.Run("blind_to_"+c.side+"_in_"+strings.ReplaceAll(c.dir, "/", "_"), func(t *testing.T) {
			inDir := func(u use) bool {
				file := u.at[:strings.LastIndex(u.at, ":")]
				return filepath.ToSlash(filepath.Dir(file)) == c.dir
			}
			blind := base
			sites, floor := &blind.reads, "read sites"
			if c.side == "writes" {
				sites, floor = &blind.writes, "write sites"
			}
			before := len(*sites)
			*sites = slices.DeleteFunc(slices.Clone(*sites), inDir)
			if len(*sites) == before {
				t.Fatalf("no %s are in %s, so this case removes nothing", c.side, c.dir)
			}
			err := underFloors(blind)
			if err == nil || !strings.Contains(err.Error(), floor) {
				t.Fatalf("an analysis blind to the %s in %s (%d of %d left) did not trip the %s "+
					"floor: err=%v", c.side, c.dir, len(*sites), before, floor, err)
			}
		})
	}
	t.Run("a_positive_control_loses_its_writer", func(t *testing.T) {
		for _, f := range positiveControls {
			lost := base
			lost.writes = slices.DeleteFunc(slices.Clone(base.writes), func(u use) bool { return u.field == f })
			if err := underFloors(lost); err != nil {
				t.Fatalf("without %s's writes the floors already fail, so this case does not "+
					"isolate the positive-control check: %v", f, err)
			}
			if err := missingPositiveControl(lost); err == nil {
				t.Errorf("an analysis that no longer sees %s's writer passed the positive controls", f)
			}
		}
	})
	t.Run("a_tree_that_does_not_type_check_is_refused", func(t *testing.T) {
		src, err := os.ReadFile(base.structFile)
		if err != nil {
			t.Fatal(err)
		}
		broken := slices.Concat(src, []byte("\nvar _ int = \"fieldguard\"\n"))
		_, err = load(root, map[string][]byte{base.structFile: broken})
		if err == nil || !strings.Contains(err.Error(), "does not load cleanly") {
			t.Fatalf("a type error in %s was not refused: err=%v", base.structFile, err)
		}
	})
}

// TestGuardControlWriteSide mutates the real tree in memory and requires the
// guard to notice. The site is picked from the analysis -- a field that is
// read somewhere and written by exactly one plain assignment -- so the
// controls keep working when the projection code moves. The second case is
// the one that matters: the same field name written on a DIFFERENT struct,
// which is how probatorium#395's FDCount hid from a name search. The third
// keeps the write and makes it a constant zero, which feeds nothing.
func TestGuardControlWriteSide(t *testing.T) {
	root := moduleRoot(t)
	base := analyse(t, root, nil)
	target := soleAssignment(t, base)
	typ := base.fieldTypes[target.field]
	t.Logf("mutating the only write of Snapshot.%s (%s), at %s", target.field, typ, target.at)

	src, err := os.ReadFile(target.file)
	if err != nil {
		t.Fatalf("read %s: %v", target.file, err)
	}
	lhs := string(src[target.lhs[0]:target.lhs[1]])
	rhs := string(src[target.rhs[0]:target.rhs[1]])
	cases := []struct{ name, stmt string }{
		{"writer_removed", "_ = " + rhs},
		{"writer_moved_to_a_lookalike_struct", fmt.Sprintf(
			"shadow395 := struct{ %s %s }{}; shadow395.%s = %s; _ = shadow395",
			target.field, typ, target.field, rhs)},
		{"writer_stores_a_constant_zero", fmt.Sprintf("_ = %s; %s = %s",
			rhs, lhs, base.fieldZeros[target.field])},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mutated := slices.Concat(src[:target.stmt[0]], []byte(c.stmt), src[target.stmt[1]:])
			got := newlyReported(analyse(t, root, map[string][]byte{target.file: mutated}), base)
			if !slices.Equal(got, []string{target.field}) {
				t.Fatalf("after replacing %q with %q the guard newly reports %v, want exactly [%s]",
					string(src[target.stmt[0]:target.stmt[1]]), c.stmt, got, target.field)
			}
		})
	}
}

// soleAssignment picks, by field name, the first Snapshot field that is read
// somewhere and written at exactly one place, that place being a plain
// assignment the controls can rewrite.
func soleAssignment(t *testing.T, a analysis) assignment {
	t.Helper()
	writes := map[string]int{}
	for _, w := range a.writes {
		writes[w.field]++
	}
	read := fieldSet(a.reads)
	var candidates []assignment
	for _, as := range a.assignments {
		if writes[as.field] == 1 && read[as.field] && a.fieldZeros[as.field] != "" {
			candidates = append(candidates, as)
		}
	}
	if len(candidates) == 0 {
		t.Fatalf("no Snapshot field is read and written by exactly one plain assignment; " +
			"the write-side controls need one to mutate")
	}
	return slices.MinFunc(candidates, func(x, y assignment) int { return strings.Compare(x.field, y.field) })
}

// TestGuardControlReadSide declares a field nothing feeds and reads it one
// way at a time. Each way a predicate really reaches a Snapshot -- a range
// over Context.History, an index into it, a struct-field chain -- must be
// seen, or a field read only that way is a silent pass; so must a read in a
// magefile. A field that is only ever set to a constant zero, or only by a
// test, must be reported too. The remaining cases prove the probe is not
// reported for its own sake: fed, or never read as a Snapshot field, it must
// stay quiet.
func TestGuardControlReadSide(t *testing.T) {
	root := moduleRoot(t)
	base := analyse(t, root, nil)
	if base.structFile == "" {
		t.Fatal("did not find the declaration of Snapshot to add the probe field to")
	}
	src, err := os.ReadFile(base.structFile)
	if err != nil {
		t.Fatalf("read %s: %v", base.structFile, err)
	}
	const probe = "Guard395Probe"
	withProbe := slices.Concat(src[:base.structClose], []byte("\t"+probe+" int64\n"), src[base.structClose:])
	probeDir := filepath.Dir(base.structFile)

	// code goes in a new file of package properties; testCode in a new test
	// file of it; mageCode in a new //go:build mage file of the root package.
	cases := []struct {
		name, code, testCode, mageCode string
		reported                       bool
	}{
		{name: "read_by_range_over_ctx_History", code: `
func fieldguardProbe(ctx Context) (n int64) {
	for _, h := range ctx.History {
		n += h.Guard395Probe
	}
	return n
}`, reported: true},
		{name: "read_by_index_into_ctx_History", code: `
func fieldguardProbe(ctx Context) int64 {
	return ctx.History[len(ctx.History)-1].Guard395Probe
}`, reported: true},
		{name: "read_through_a_struct_field_chain", code: `
type fieldguardHolder struct{ inner struct{ last Snapshot } }

func fieldguardProbe(h *fieldguardHolder) int64 { return h.inner.last.Guard395Probe }`, reported: true},
		{name: "read_through_a_slice_parameter", code: `
func fieldguardProbe(hs []Snapshot) int64 { return hs[0].Guard395Probe }`, reported: true},
		{name: "read_through_a_map_of_pointers", code: `
func fieldguardProbe(m map[string]*Snapshot) int64 { return m["last"].Guard395Probe }`, reported: true},
		{name: "read_as_a_promoted_field", code: `
type fieldguardWrap struct{ *Snapshot }

func fieldguardProbe(w fieldguardWrap) int64 { return w.Guard395Probe }`, reported: true},
		{name: "read_through_atomic_Load_of_its_address", code: `
import "sync/atomic"

func fieldguardProbe(s *Snapshot) int64 { return atomic.LoadInt64(&s.Guard395Probe) }`, reported: true},

		{name: "read_in_a_mage_tagged_file", mageCode: `
import "github.com/goceleris/probatorium/validation/properties"

func fieldguardProbe(s *properties.Snapshot) int64 { return s.Guard395Probe }`, reported: true},
		{name: "fed_only_by_a_test_file", code: `
func fieldguardProbe(s *Snapshot) int64 { return s.Guard395Probe }`, testCode: `
func init() {
	var s Snapshot
	s.Guard395Probe = 1
	_ = s
}`, reported: true},
		{name: "set_only_to_a_constant_zero", code: `
import "sync/atomic"

const fieldguardNone = 0

func fieldguardProbe(ctx Context, s *Snapshot) int64 {
	ctx.History[0].Guard395Probe = 0
	s.Guard395Probe = fieldguardNone
	atomic.StoreInt64(&s.Guard395Probe, 0)
	fresh := Snapshot{Guard395Probe: 0}
	return fresh.Guard395Probe + s.Guard395Probe
}`, reported: true},

		{name: "fed_by_an_assignment_into_History", code: `
func fieldguardProbe(ctx Context) int64 {
	ctx.History[0].Guard395Probe = 1
	return ctx.History[0].Guard395Probe
}`, reported: false},
		{name: "fed_by_an_increment", code: `
func fieldguardProbe(s *Snapshot) int64 {
	s.Guard395Probe++
	return s.Guard395Probe
}`, reported: false},
		{name: "fed_by_the_second_operand_of_a_tuple_assignment", code: `
func fieldguardPair() (int, int64) { return 1, 2 }

func fieldguardProbe(s *Snapshot) int64 {
	_, s.Guard395Probe = fieldguardPair()
	return s.Guard395Probe
}`, reported: false},
		{name: "fed_by_a_composite_literal", code: `
func fieldguardProbe() int64 {
	s := Snapshot{Guard395Probe: 1}
	return s.Guard395Probe
}`, reported: false},
		{name: "fed_by_atomic_Add", code: `
import "sync/atomic"

func fieldguardProbe(s *Snapshot) int64 {
	atomic.AddInt64(&s.Guard395Probe, 1)
	return s.Guard395Probe
}`, reported: false},
		{name: "a_lookalike_read_is_not_a_Snapshot_read", code: `
func fieldguardProbe() int64 {
	var x struct{ Guard395Probe int64 }
	return x.Guard395Probe
}`, reported: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			overlay := map[string][]byte{base.structFile: withProbe}
			if c.code != "" {
				overlay[filepath.Join(probeDir, "zz_fieldguard_probe.go")] =
					[]byte("package properties\n" + c.code + "\n")
			}
			if c.testCode != "" {
				overlay[filepath.Join(probeDir, "zz_fieldguard_probe_test.go")] =
					[]byte("package properties\n" + c.testCode + "\n")
			}
			if c.mageCode != "" {
				overlay[filepath.Join(root, "zz_fieldguard_probe_mage.go")] =
					[]byte("//go:build mage\n\npackage main\n" + c.mageCode + "\n")
			}
			a := analyse(t, root, overlay)
			if _, ok := a.fieldTypes[probe]; !ok {
				t.Fatalf("the overlay did not add %s to Snapshot, so this case tests nothing", probe)
			}
			got := newlyReported(a, base)
			var want []string
			if c.reported {
				want = []string{probe}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("the guard newly reports %v, want %v, for:%s%s%s", got, want, c.code, c.testCode, c.mageCode)
			}
		})
	}
}
