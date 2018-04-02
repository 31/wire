// Package goose provides compile-time dependency injection logic as a
// Go library.
package goose

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/types"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/go/loader"
)

// Generate performs dependency injection for a single package,
// returning the gofmt'd Go source code.
func Generate(bctx *build.Context, wd string, pkg string) ([]byte, error) {
	// TODO(light): allow errors
	// TODO(light): stop errors from printing to stderr
	conf := &loader.Config{
		Build:               new(build.Context),
		ParserMode:          parser.ParseComments,
		Cwd:                 wd,
		TypeCheckFuncBodies: func(string) bool { return false },
	}
	*conf.Build = *bctx
	n := len(conf.Build.BuildTags)
	conf.Build.BuildTags = append(conf.Build.BuildTags[:n:n], "gooseinject")
	conf.Import(pkg)
	prog, err := conf.Load()
	if err != nil {
		return nil, fmt.Errorf("load: %v", err)
	}
	if len(prog.InitialPackages()) != 1 {
		// This is more of a violated precondition than anything else.
		return nil, fmt.Errorf("load: got %d packages", len(prog.InitialPackages()))
	}
	pkgInfo := prog.InitialPackages()[0]
	g := newGen(prog, pkgInfo.Pkg.Path())
	mc := newProviderSetCache(prog)
	var directives []directive
	for _, f := range pkgInfo.Files {
		if !isInjectFile(f) {
			continue
		}
		// TODO(light): use same directive extraction logic as provider set finding.
		fileScope := pkgInfo.Scopes[f]
		cmap := ast.NewCommentMap(prog.Fset, f, f.Comments)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			directives = directives[:0]
			for _, c := range cmap[fn] {
				directives = extractDirectives(directives, c)
			}
			sets := make([]providerSetRef, 0, len(directives))
			for _, d := range directives {
				if d.kind != "use" {
					return nil, fmt.Errorf("%v: cannot use %s directive on inject function", prog.Fset.Position(d.pos), d.kind)
				}
				ref, err := parseProviderSetRef(d.line, fileScope, g.currPackage, d.pos)
				if err != nil {
					return nil, fmt.Errorf("%v: %v", prog.Fset.Position(d.pos), err)
				}
				sets = append(sets, ref)
			}
			sig := pkgInfo.ObjectOf(fn.Name).Type().(*types.Signature)
			if err := g.inject(mc, fn.Name.Name, sig, sets); err != nil {
				return nil, fmt.Errorf("%v: %v", prog.Fset.Position(fn.Pos()), err)
			}
		}
	}
	goSrc := g.frame()
	fmtSrc, err := format.Source(goSrc)
	if err != nil {
		// This is likely a bug from a poorly generated source file.
		// Return an error and the unformatted source.
		return goSrc, err
	}
	return fmtSrc, nil
}

// gen is the generator state.
type gen struct {
	currPackage string
	buf         bytes.Buffer
	imports     map[string]string
	prog        *loader.Program // for determining package names
}

func newGen(prog *loader.Program, pkg string) *gen {
	return &gen{
		currPackage: pkg,
		imports:     make(map[string]string),
		prog:        prog,
	}
}

// frame bakes the built up source body into an unformatted Go source file.
func (g *gen) frame() []byte {
	if g.buf.Len() == 0 {
		return nil
	}
	var buf bytes.Buffer
	buf.WriteString("// Code generated by goose. DO NOT EDIT.\n\n//+build !gooseinject\n\npackage ")
	buf.WriteString(g.prog.Package(g.currPackage).Pkg.Name())
	buf.WriteString("\n\n")
	if len(g.imports) > 0 {
		buf.WriteString("import (\n")
		imps := make([]string, 0, len(g.imports))
		for path := range g.imports {
			imps = append(imps, path)
		}
		sort.Strings(imps)
		for _, path := range imps {
			// TODO(light): Omit the local package identifier if it matches
			// the package name.
			fmt.Fprintf(&buf, "\t%s %q\n", g.imports[path], path)
		}
		buf.WriteString(")\n\n")
	}
	buf.Write(g.buf.Bytes())
	return buf.Bytes()
}

// inject emits the code for an injector.
func (g *gen) inject(mc *providerSetCache, name string, sig *types.Signature, sets []providerSetRef) error {
	results := sig.Results()
	returnsErr := false
	switch results.Len() {
	case 0:
		return fmt.Errorf("inject %s: no return values", name)
	case 1:
		// nothing special
	case 2:
		if t := results.At(1).Type(); !types.Identical(t, errorType) {
			return fmt.Errorf("inject %s: second return type is %s; must be error", name, types.TypeString(t, nil))
		}
		returnsErr = true
	default:
		return fmt.Errorf("inject %s: too many return values", name)
	}
	outType := results.At(0).Type()
	params := sig.Params()
	given := make([]types.Type, params.Len())
	for i := 0; i < params.Len(); i++ {
		given[i] = params.At(i).Type()
	}
	calls, err := solve(mc, outType, given, sets)
	if err != nil {
		return err
	}
	for i := range calls {
		if calls[i].hasErr && !returnsErr {
			return fmt.Errorf("inject %s: provider for %s returns error but injection not allowed to fail", name, types.TypeString(calls[i].out, nil))
		}
	}

	// Prequalify all types.  Since import disambiguation ignores local
	// variables, it takes precedence.
	paramTypes := make([]string, params.Len())
	for i := 0; i < params.Len(); i++ {
		paramTypes[i] = types.TypeString(params.At(i).Type(), g.qualifyPkg)
	}
	for _, c := range calls {
		g.qualifyImport(c.importPath)
	}
	outTypeString := types.TypeString(outType, g.qualifyPkg)
	zv := zeroValue(outType, g.qualifyPkg)
	// Set up local variables
	paramNames := make([]string, params.Len())
	localNames := make([]string, len(calls))
	errVar := disambiguate("err", g.nameInFileScope)
	collides := func(v string) bool {
		if v == errVar {
			return true
		}
		for _, a := range paramNames {
			if a == v {
				return true
			}
		}
		for _, l := range localNames {
			if l == v {
				return true
			}
		}
		return g.nameInFileScope(v)
	}

	g.p("func %s(", name)
	for i := 0; i < params.Len(); i++ {
		if i > 0 {
			g.p(", ")
		}
		pi := params.At(i)
		a := pi.Name()
		if a == "" || a == "_" {
			a = typeVariableName(pi.Type())
			if a == "" {
				a = "arg"
			}
		}
		paramNames[i] = disambiguate(a, collides)
		g.p("%s %s", paramNames[i], paramTypes[i])
	}
	if returnsErr {
		g.p(") (%s, error) {\n", outTypeString)
	} else {
		g.p(") %s {\n", outTypeString)
	}
	for i := range calls {
		c := &calls[i]
		lname := typeVariableName(c.out)
		if lname == "" {
			lname = "v"
		}
		lname = disambiguate(lname, collides)
		localNames[i] = lname
		g.p("\t%s", lname)
		if c.hasErr {
			g.p(", %s", errVar)
		}
		g.p(" := %s(", g.qualifiedID(c.importPath, c.funcName))
		for j, a := range c.args {
			if j > 0 {
				g.p(", ")
			}
			if a < params.Len() {
				g.p("%s", paramNames[a])
			} else {
				g.p("%s", localNames[a-params.Len()])
			}
		}
		g.p(")\n")
		if c.hasErr {
			g.p("\tif %s != nil {\n", errVar)
			// TODO(light): give information about failing provider
			g.p("\t\treturn %s, err\n", zv)
			g.p("\t}\n")
		}
	}
	if len(calls) == 0 {
		for i := range given {
			if types.Identical(outType, given[i]) {
				g.p("\treturn %s", paramNames[i])
				break
			}
		}
	} else {
		g.p("\treturn %s", localNames[len(calls)-1])
	}
	if returnsErr {
		g.p(", nil")
	}
	g.p("\n}\n")
	return nil
}

func (g *gen) qualifiedID(path, sym string) string {
	name := g.qualifyImport(path)
	if name == "" {
		return sym
	}
	return name + "." + sym
}

func (g *gen) qualifyImport(path string) string {
	if path == g.currPackage {
		return ""
	}
	if name := g.imports[path]; name != "" {
		return name
	}
	// TODO(light): use parts of import path to disambiguate.
	name := disambiguate(g.prog.Package(path).Pkg.Name(), func(n string) bool {
		// Don't let an import take the "err" name. That's annoying.
		return n == "err" || g.nameInFileScope(n)
	})
	g.imports[path] = name
	return name
}

func (g *gen) nameInFileScope(name string) bool {
	for _, other := range g.imports {
		if other == name {
			return true
		}
	}
	_, obj := g.prog.Package(g.currPackage).Pkg.Scope().LookupParent(name, 0)
	return obj != nil
}

func (g *gen) qualifyPkg(pkg *types.Package) string {
	return g.qualifyImport(pkg.Path())
}

func (g *gen) p(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

// zeroValue returns the shortest expression that evaluates to the zero
// value for the given type.
func zeroValue(t types.Type, qf types.Qualifier) string {
	switch u := t.Underlying().(type) {
	case *types.Array, *types.Struct:
		return types.TypeString(t, qf) + "{}"
	case *types.Basic:
		info := u.Info()
		switch {
		case info&types.IsBoolean != 0:
			return "false"
		case info&(types.IsInteger|types.IsFloat|types.IsComplex) != 0:
			return "0"
		case info&types.IsString != 0:
			return `""`
		default:
			panic("unreachable")
		}
	case *types.Chan, *types.Interface, *types.Map, *types.Pointer, *types.Signature, *types.Slice:
		return "nil"
	default:
		panic("unreachable")
	}
}

// typeVariableName invents a variable name derived from the type name
// or returns the empty string if one could not be found.
func typeVariableName(t types.Type) string {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	tn, ok := t.(*types.Named)
	if !ok {
		return ""
	}
	// TODO(light): include package name when appropriate
	return unexport(tn.Obj().Name())
}

// unexport converts a name that is potentially exported to an unexported name.
func unexport(name string) string {
	r, sz := utf8.DecodeRuneInString(name)
	if !unicode.IsUpper(r) {
		// foo -> foo
		return name
	}
	r2, sz2 := utf8.DecodeRuneInString(name[sz:])
	if !unicode.IsUpper(r2) {
		// Foo -> foo
		return string(unicode.ToLower(r)) + name[sz:]
	}
	// UPPERWord -> upperWord
	sbuf := new(strings.Builder)
	sbuf.WriteRune(unicode.ToLower(r))
	i := sz
	r, sz = r2, sz2
	for unicode.IsUpper(r) && sz > 0 {
		r2, sz2 := utf8.DecodeRuneInString(name[i+sz:])
		if sz2 > 0 && unicode.IsLower(r2) {
			break
		}
		i += sz
		sbuf.WriteRune(unicode.ToLower(r))
		r, sz = r2, sz2
	}
	sbuf.WriteString(name[i:])
	return sbuf.String()
}

// disambiguate picks a unique name, preferring name if it is already unique.
func disambiguate(name string, collides func(string) bool) string {
	if !collides(name) {
		return name
	}
	buf := []byte(name)
	if len(buf) > 0 && buf[len(buf)-1] >= '0' && buf[len(buf)-1] <= '9' {
		buf = append(buf, '_')
	}
	base := len(buf)
	for n := 2; ; n++ {
		buf = strconv.AppendInt(buf[:base], int64(n), 10)
		sbuf := string(buf)
		if !collides(sbuf) {
			return sbuf
		}
	}
}

var errorType = types.Universe.Lookup("error").Type()
