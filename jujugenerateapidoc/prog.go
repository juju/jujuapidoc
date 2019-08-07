// The generateapidoc program is bundled as an asset into jujuapidoc
// so that we don't need to remember to compile that program
// in order to generate the docs.
package main

// see github.com/juju/juju 076-apiserver-facade-list-details

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"html/template"
	"log"
	"os"
	"reflect"

	// These dependencies should not be put in the
	// go.mod file, as they should come from the
	// selected juju version.
	"github.com/juju/errors"
	"github.com/juju/juju/apiserver"
	"github.com/juju/juju/apiserver/common"
	"github.com/juju/juju/apiserver/facade"
	"github.com/juju/juju/permission"
	"github.com/juju/juju/state"
	"github.com/juju/rpcreflect"
	"gopkg.in/juju/names.v2"

	"github.com/juju/jujuapidoc/apidoc"
	"github.com/rogpeppe/apicompat/jsontypes"
	"golang.org/x/tools/go/packages"
	"gopkg.in/errgo.v1"
)

func main() {
	info, err := generateInfo()
	if err != nil {
		log.Fatal(err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		log.Fatal(err)
	}
	os.Stdout.Write(data)
	if len(panicked) > 0 {
		log.Printf("%d/%d facades panicked when trying to determine access (this is normal)", len(panicked), len(allFacadeNames))
	}
}

func generateInfo() (*apidoc.Info, error) {
	cfg := packages.Config{
		Mode: packages.LoadAllSyntax,
		ParseFile: func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
			return parser.ParseFile(fset, filename, src, parser.ParseComments)
		},
	}
	serverPkg := "github.com/juju/juju/apiserver"
	pkgs, err := packages.Load(&cfg, serverPkg)
	if err != nil {
		return nil, errgo.Notef(err, "cannot load %q", serverPkg)
	}
	if len(pkgs) != 1 {
		return nil, errgo.Newf("packages.Load returned %d packages, not 1", len(pkgs))
	}
	pkg := pkgs[0]

	info := jsontypes.NewInfo()
	ds := apiserver.AllFacades().ListDetails()
	ds = append(ds, apiserver.AdminFacadeDetails()...)
	for _, d := range ds {
		t := rpcreflect.ObjTypeOf(d.Type)

		for _, name := range t.MethodNames() {
			m, _ := t.Method(name)
			if m.Params != nil {
				info.TypeInfo(m.Params)
			}
			if m.Result != nil {
				info.TypeInfo(m.Result)
			}
		}
	}
	apiInfo := &apidoc.Info{
		TypeInfo: info,
	}
	for _, d := range ds {
		f := apidoc.FacadeInfo{
			Name:        d.Name,
			Version:     d.Version,
			AvailableTo: availableTo(d.Name, d.Factory),
		}
		pt, err := progType(pkg, d.Type)
		if err != nil {
			return nil, errgo.Notef(err, "cannot get prog type for %v", d.Type)
		}
		tdoc, err := typeDocComment(pkg, pt)
		if err != nil {
			return nil, errgo.Notef(err, "cannot get doc comment for %v: %v", d.Type)
		}
		f.Doc = tdoc
		t := rpcreflect.ObjTypeOf(d.Type)
		for _, name := range t.MethodNames() {
			m, _ := t.Method(name)
			fm := apidoc.Method{
				Name: name,
			}
			if m.Params != nil {
				fm.Param = info.Ref(m.Params)
			}
			if m.Result != nil {
				fm.Result = info.Ref(m.Result)
			}
			mdoc, err := methodDocComment(pkg, pt, name)
			if err != nil {
				return nil, errgo.Notef(err, "cannot get doc comment for %v.%v: %v", d.Type, name)
			}
			fm.Doc = mdoc
			f.Methods = append(f.Methods, fm)
		}
		apiInfo.Facades = append(apiInfo.Facades, f)
	}
	return apiInfo, nil
}

var tmplFuncs = template.FuncMap{
	"typeLink": func(t *jsontypes.Type) template.HTML {
		if t == nil {
			return "n/a"
		}
		link := fmt.Sprintf(`<a href="https://godoc.org/%s">%s</a>`, t.Name, t.Name.Name())
		return template.HTML(link)
	},
}

func methodDocComment(pkg *packages.Package, tname *types.TypeName, methodName string) (string, error) {
	t := tname.Type()
	if !types.IsInterface(t) {
		// Use the pointer type to get as many methods as possible.
		t = types.NewPointer(t)
	}

	mset := types.NewMethodSet(t)
	sel := mset.Lookup(nil, methodName)
	if sel == nil {
		return "", errgo.Newf("cannot find method %v on %v", methodName, t)
	}
	obj := sel.Obj()
	decl, err := findDecl(pkg, obj.Pos())
	if err != nil {
		return "", errgo.Mask(err)
	}
	switch decl := decl.(type) {
	case *ast.GenDecl:
		if decl.Tok != token.TYPE {
			return "", errgo.Newf("found non-type decl %#v", decl)
		}
		for _, spec := range decl.Specs {
			tspec := spec.(*ast.TypeSpec)
			it := tspec.Type.(*ast.InterfaceType)
			for _, m := range it.Methods.List {
				for _, id := range m.Names {
					if id.Pos() == obj.Pos() {
						return m.Doc.Text(), nil
					}
				}
			}
		}
		return "", errgo.Newf("method definition not found in type")
	case *ast.FuncDecl:
		if decl.Name.Pos() != obj.Pos() {
			return "", errgo.Newf("method definition not found (at %#v)", pkg.Fset.Position(obj.Pos()))
		}
		return decl.Doc.Text(), nil
	default:
		return "", errgo.Newf("unexpected declaration %T found", decl)
	}
}

func typeDocComment(pkg *packages.Package, t *types.TypeName) (string, error) {
	decl, err := findDecl(pkg, t.Pos())
	if err != nil {
		return "", errgo.Mask(err)
	}
	tdecl, ok := decl.(*ast.GenDecl)
	if !ok || tdecl.Tok != token.TYPE {
		return "", errgo.Newf("found non-type decl %#v", decl)
	}
	for _, spec := range tdecl.Specs {
		tspec := spec.(*ast.TypeSpec)
		if tspec.Name.Pos() == t.Pos() {
			if tspec.Doc != nil {
				return tspec.Doc.Text(), nil
			}
			return tdecl.Doc.Text(), nil
		}
	}
	return "", errgo.Newf("cannot find type declaration")
}

// findDecl returns the top level declaration that contains the
// given position.
func findDecl(pkg *packages.Package, pos token.Pos) (ast.Decl, error) {
	tokFile := pkg.Fset.File(pos)
	if tokFile == nil {
		return nil, errgo.Newf("no file found for object")
	}
	filename := tokFile.Name()
	var found ast.Decl
	packages.Visit([]*packages.Package{pkg}, func(pkg *packages.Package) bool {
		for _, f := range pkg.Syntax {
			if tokFile := pkg.Fset.File(f.Pos()); tokFile == nil || tokFile.Name() != filename {
				continue
			}
			// We've found the file we're looking for. Now traverse all
			// top level declarations looking for the right function declaration.
			for _, decl := range f.Decls {
				if decl.Pos() <= pos && pos <= decl.End() {
					found = decl
					return false
				}
			}
		}
		return true
	}, nil)
	if found == nil {
		return nil, errgo.Newf("declaration not found")
	}
	return found, nil
}

// progType returns the go/types type for the given reflect.Type,
// which must represent a named non-predeclared Go type.
func progType(pkg *packages.Package, t reflect.Type) (*types.TypeName, error) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	typeName := t.Name()
	if typeName == "" {
		return nil, errgo.Newf("type %s is not named", t)
	}
	pkgPath := t.PkgPath()
	if pkgPath == "" {
		// TODO could return types.Basic type here if we needed to.
		return nil, errgo.Newf("type %s not declared in package", t)
	}
	var found *packages.Package
	packages.Visit([]*packages.Package{pkg}, func(pkg *packages.Package) bool {
		if pkg.PkgPath == pkgPath {
			found = pkg
			return false
		}
		return true
	}, nil)
	if found == nil {
		return nil, errgo.Newf("cannot find %q in imported code", pkgPath)
	}

	obj := found.Types.Scope().Lookup(typeName)
	if obj == nil {
		return nil, errgo.Newf("type %s not found in %s", typeName, pkgPath)
	}
	objTypeName, ok := obj.(*types.TypeName)
	if !ok {
		return nil, errgo.Newf("%s is not a type", typeName)
	}
	return objTypeName, nil
}

func availableTo(facadeName string, factory facade.Factory) []string {
	var a []string
	for i, kindStr := range kinds {
		if isAvailable(facadeName, factory, entityKind(i)) {
			a = append(a, kindStr)
		}
	}
	return a
}

var (
	allFacadeNames = make(map[string]bool)
	panicked       = make(map[string]bool)
)

func isAvailable(facadeName string, factory facade.Factory, kind entityKind) (ok bool) {
	if factory == nil {
		// Admin facade only.
		return true
	}
	if kind == kindControllerUser && !apiserver.IsControllerFacade(facadeName) {
		return false
	}
	if kind == kindModelUser && !apiserver.IsModelFacade(facadeName) {
		return false
	}
	allFacadeNames[facadeName] = true
	defer func() {
		err := recover()
		if err == nil {
			return
		}
		//log.Printf("panic on facade %q, role %v (%v): %s", facadeName, kind, err, debug.Callers(0, 30))
		panicked[facadeName] = true
		ok = true
	}()
	ctx := context{
		auth: authorizer{
			kind: kind,
		},
	}
	_, err := factory(ctx)
	return errors.Cause(err) != common.ErrPerm
}

type entityKind int

const (
	kindControllerMachine = entityKind(iota)
	kindMachineAgent
	kindUnitAgent
	kindControllerUser
	kindModelUser
)

func (k entityKind) String() string {
	return kinds[k]
}

var kinds = []string{
	kindControllerMachine: "controller-machine-agent",
	kindMachineAgent:      "machine-agent",
	kindUnitAgent:         "unit-agent",
	kindControllerUser:    "controller-user",
	kindModelUser:         "model-user",
}

type context struct {
	auth authorizer
	facade.Context
}

func (c context) Auth() facade.Authorizer {
	return c.auth
}

func (c context) ID() string {
	return ""
}

func (c context) State() *state.State {
	return new(state.State)
}

func (c context) Resources() facade.Resources {
	return nil
}

func (c context) StatePool() *state.StatePool {
	return new(state.StatePool)
}

func (c context) ControllerTag() names.ControllerTag {
	return names.NewControllerTag("xxxx")
}

type authorizer struct {
	facade.Authorizer
	kind entityKind
}

func (a authorizer) AuthController() bool {
	return a.kind == kindControllerMachine
}

func (a authorizer) HasPermission(operation permission.Access, target names.Tag) (bool, error) {
	return true, nil
}

func (a authorizer) AuthMachineAgent() bool {
	return a.kind == kindMachineAgent || a.kind == kindControllerMachine
}

func (a authorizer) AuthUnitAgent() bool {
	return a.kind == kindUnitAgent
}

func (a authorizer) AuthClient() bool {
	return a.kind == kindControllerUser || a.kind == kindModelUser
}

func (a authorizer) GetAuthTag() names.Tag {
	switch a.kind {
	case kindControllerUser, kindModelUser:
		return names.NewUserTag("bob")
	case kindUnitAgent:
		return names.NewUnitTag("xx/0")
	case kindMachineAgent, kindControllerMachine:
		return names.NewMachineTag("0")
	}
	panic("unknown kind")
}
