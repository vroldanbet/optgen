package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	_ "github.com/creasty/defaults"
	"github.com/dave/jennifer/jen"
	"github.com/fatih/structtag"
	"golang.org/x/tools/go/packages"
)

type WriterProvider func() io.Writer

// TODO: struct tags to know what to generate
// TODO: recursive generation, i.e. WithMetadata(WithName())
// TODO: optional flattening of recursive generation, i.e. WithMetadataName()
// TODO: configurable field prefix
// TODO: exported / unexported generation

var DefaultSensitiveNames = "secure"

func main() {
	fs := flag.NewFlagSet("optgen", flag.ContinueOnError)
	outputPathFlag := fs.String(
		"output",
		"",
		"Location where generated options will be written",
	)
	pkgNameFlag := fs.String(
		"package",
		"",
		"Name of package to use in output file",
	)
	sensitiveFieldNamesFlag := fs.String(
		"sensitive-field-name-matches",
		DefaultSensitiveNames,
		"Substring matches of field names that should be considered sensitive",
	)

	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatal(err.Error())
	}

	if len(fs.Args()) < 2 {
		// TODO: usage
		log.Fatal("must specify a package directory and a struct to provide options for")
	}

	pkgName := fs.Arg(0)
	structNames := fs.Args()[1:]
	structFilter := make(map[string]struct{}, len(structNames))
	for _, structName := range structNames {
		structFilter[structName] = struct{}{}
	}

	var writer WriterProvider
	if outputPathFlag != nil {
		writer = func() io.Writer {
			w, err := os.OpenFile(*outputPathFlag, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
			if err != nil {
				log.Fatalf("couldn't open %s for writing", *outputPathFlag)
			}
			return w
		}
	}

	packagePath, packageName := func() (string, string) {
		cfg := &packages.Config{
			Mode: packages.NeedTypes | packages.NeedTypesInfo,
		}
		pkgs, err := packages.Load(cfg, path.Dir(*outputPathFlag))
		if err != nil {
			fmt.Fprintf(os.Stderr, "load: %v\n", err)
			os.Exit(1)
		}
		if packages.PrintErrors(pkgs) > 0 {
			os.Exit(1)
		}
		return pkgs[0].Types.Path(), pkgs[0].Types.Name()
	}()
	if pkgNameFlag != nil && *pkgNameFlag != "" {
		packageName = *pkgNameFlag
	}

	sensitiveNameMatches := make([]string, 0)
	if sensitiveFieldNamesFlag != nil {
		sensitiveNameMatches = strings.Split(*sensitiveFieldNamesFlag, ",")
	}

	err := func() error {
		cfg := &packages.Config{
			Mode: packages.NeedFiles | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedSyntax,
		}
		pkgs, err := packages.Load(cfg, pkgName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load: %v\n", err)
			os.Exit(1)
		}
		if packages.PrintErrors(pkgs) > 0 {
			os.Exit(1)
		}

		count := 0
		for _, pkg := range pkgs {
			for _, f := range pkg.Syntax {
				structs := findStructDefs(f, pkg.TypesInfo.Defs, structFilter)
				if len(structs) == 0 {
					continue
				}
				fmt.Printf("Generating options for %s.%s...\n", packageName, strings.Join(structNames, ", "))
				err = generateForFile(structs, packagePath, packageName, f.Name.Name, *outputPathFlag, sensitiveNameMatches, writer)
				if err != nil {
					return err
				}
				count++
			}
		}
		fmt.Printf("Generated %d options\n", count)

		return nil
	}()
	if err != nil {
		log.Fatal(err)
	}
}

func findStructDefs(file *ast.File, defs map[*ast.Ident]types.Object, names map[string]struct{}) []types.Object {
	found := make([]*ast.TypeSpec, 0)
	ast.Inspect(file, func(node ast.Node) bool {
		var ts *ast.TypeSpec
		var ok bool

		if ts, ok = node.(*ast.TypeSpec); !ok {
			return true
		}

		if ts.Name == nil {
			return true
		}

		if _, ok := names[ts.Name.Name]; !ok {
			return false
		}
		found = append(found, ts)

		return false
	})

	if len(found) == 0 {
		return nil
	}

	objs := make([]types.Object, 0)
	for _, s := range found {
		switch s.Type.(type) {
		case *ast.StructType:
			def, ok := defs[s.Name]
			if !ok {
				continue
			}
			objs = append(objs, def)
		}
	}
	return objs
}

type Config struct {
	ReceiverId     string
	OptTypeName    string
	TargetTypeName string
	StructRef      []jen.Code
	StructName     string
	PkgPath        string
}

func generateForFile(objs []types.Object, pkgPath, pkgName, fileName, outpath string, sensitiveNameMatches []string, writer WriterProvider) error {
	outdir, err := filepath.Abs(filepath.Dir(outpath))
	if err != nil {
		return err
	}

	buf := jen.NewFilePathName(outpath, pkgName)
	buf.PackageComment("Code generated by github.com/ecordell/optgen. DO NOT EDIT.")

	for _, def := range objs {
		st, ok := def.Type().Underlying().(*types.Struct)
		if !ok {
			return errors.New("type is not a struct")
		}

		config := Config{
			ReceiverId:     strings.ToLower(string(def.Name()[0])),
			OptTypeName:    fmt.Sprintf("%sOption", def.Name()),
			TargetTypeName: strings.Title(def.Name()),
			StructRef:      []jen.Code{jen.Id(def.Name())},
			StructName:     def.Name(),
			PkgPath:        pkgPath,
		}

		// if output is not to the same package, qualify imports
		structPkg := st.Field(0).Pkg().Path()
		if pkgPath != structPkg {
			config.StructRef = []jen.Code{jen.Qual(structPkg, def.Name())}
			config.StructName = jen.Qual(structPkg, def.Name()).GoString()
		}

		// generate the Option type
		writeOptionType(buf, config)

		// generate NewXWithOptions
		writeNewXWithOptions(buf, config)

		// generate NewXWithOptionsAndDefaults
		writeNewXWithOptionsAndDefaults(buf, config)

		// generate ToOption
		writeToOption(buf, st, config)

		// generate DebugMap
		writeDebugMap(buf, st, config, sensitiveNameMatches)

		// generate WithOptions
		writeXWithOptions(buf, config)
		writeWithOptions(buf, config)

		// generate all With* functions
		writeAllWithOptFuncs(buf, st, outdir, config)
	}

	w := writer()
	if w == nil {
		optFile := strings.Replace(fileName, ".go", "_opts.go", 1)
		w, err = os.OpenFile(optFile, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
	}

	return buf.Render(w)
}

func writeOptionType(buf *jen.File, c Config) {
	buf.Type().Id(c.OptTypeName).Func().Params(jen.Id(c.ReceiverId).Op("*").Add(c.StructRef...))
}

func writeNewXWithOptions(buf *jen.File, c Config) {
	newFuncName := fmt.Sprintf("New%sWithOptions", c.TargetTypeName)
	buf.Comment(fmt.Sprintf("%s creates a new %s with the passed in options set", newFuncName, c.StructName))
	buf.Func().Id(newFuncName).Params(
		jen.Id("opts").Op("...").Id(c.OptTypeName),
	).Op("*").Add(c.StructRef...).BlockFunc(func(grp *jen.Group) {
		grp.Id(c.ReceiverId).Op(":=").Op("&").Add(c.StructRef...).Block()
		applyOptions(c.ReceiverId)(grp)
	})
}

func writeNewXWithOptionsAndDefaults(buf *jen.File, c Config) {
	newFuncName := fmt.Sprintf("New%sWithOptionsAndDefaults", c.TargetTypeName)
	buf.Comment(fmt.Sprintf("%s creates a new %s with the passed in options set starting from the defaults", newFuncName, c.StructName))
	buf.Func().Id(newFuncName).Params(
		jen.Id("opts").Op("...").Id(c.OptTypeName),
	).Op("*").Add(c.StructRef...).BlockFunc(func(grp *jen.Group) {
		grp.Id(c.ReceiverId).Op(":=").Op("&").Add(c.StructRef...).Block()
		grp.Qual("github.com/creasty/defaults", "MustSet").Call(jen.Id(c.ReceiverId))
		applyOptions(c.ReceiverId)(grp)
	})
}

const (
	DebugMapFieldTag = "debugmap"
)

func writeDebugMap(buf *jen.File, st *types.Struct, c Config, sensitiveNameMatches []string) {
	newFuncName := fmt.Sprintf("DebugMap")

	buf.Comment(fmt.Sprintf("%s returns a map form of %s for debugging", newFuncName, c.TargetTypeName))
	buf.Func().Params(jen.Id(c.ReceiverId).Id(c.StructName)).Id(newFuncName).Params().Id("map[string]any").BlockFunc(func(grp *jen.Group) {
		mapId := "debugMap"
		grp.Id(mapId).Op(":=").Map(jen.String()).Any().Values()

		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if f.Anonymous() || !f.Exported() {
				continue
			}

			tags, err := structtag.Parse(st.Tag(i))
			if err != nil {
				panic(err)
			}

			tag, err := tags.Get(DebugMapFieldTag)
			if err != nil {
				fmt.Printf("missing debugmap tag on field %s in type %s\n", f.Name(), c.TargetTypeName)
				os.Exit(1)
			}

			switch tag.Value() {
			case "visible":
				for _, sensitiveName := range sensitiveNameMatches {
					if strings.Contains(strings.ToLower(f.Name()), sensitiveName) {
						fmt.Printf("field %s in type %s must be marked as 'sensitive'\n", f.Name(), c.TargetTypeName)
						os.Exit(1)
					}
				}

				grp.Id(mapId).Index(jen.Lit(f.Name())).Op("=").Qual("github.com/ecordell/optgen/helpers", "DebugValue").Call(jen.Id(c.ReceiverId).Dot(f.Name()), jen.Lit(false))

			case "visible-format":
				for _, sensitiveName := range sensitiveNameMatches {
					if strings.Contains(strings.ToLower(f.Name()), sensitiveName) {
						fmt.Printf("field %s in type %s must be marked as 'sensitive'\n", f.Name(), c.TargetTypeName)
						os.Exit(1)
					}
				}

				grp.Id(mapId).Index(jen.Lit(f.Name())).Op("=").Qual("github.com/ecordell/optgen/helpers", "DebugValue").Call(jen.Id(c.ReceiverId).Dot(f.Name()), jen.Lit(true))

			case "hidden":
				// skipped
				continue

			case "sensitive":
				grp.Id(mapId).Index(jen.Lit(f.Name())).Op("=").Qual("github.com/ecordell/optgen/helpers", "SensitiveDebugValue").Call(jen.Id(c.ReceiverId).Dot(f.Name()))

			default:
				fmt.Printf("unknown value '%s' for debugmap tag on field %s in type %s\n", tag.Value(), f.Name(), c.TargetTypeName)
				os.Exit(1)
			}
		}

		grp.Return(jen.Id(mapId))
	})
}

func writeToOption(buf *jen.File, st *types.Struct, c Config) {
	newFuncName := fmt.Sprintf("ToOption")

	buf.Comment(fmt.Sprintf("%s returns a new %s that sets the values from the passed in %s", newFuncName, c.OptTypeName, c.StructName))
	buf.Func().Params(jen.Id(c.ReceiverId).Op("*").Id(c.StructName)).Id(newFuncName).Params().Id(c.OptTypeName).BlockFunc(func(grp *jen.Group) {
		grp.Return(jen.Func().Params(jen.Id("to").Op("*").Id(c.StructName)).BlockFunc(func(retGrp *jen.Group) {
			for i := 0; i < st.NumFields(); i++ {
				f := st.Field(i)
				if f.Anonymous() {
					continue
				}
				retGrp.Id("to").Op(".").Id(f.Name()).Op("=").Id(c.ReceiverId).Op(".").Id(f.Name())
			}
		}))
	})
}

func writeXWithOptions(buf *jen.File, c Config) {
	withFuncName := fmt.Sprintf("%sWithOptions", c.TargetTypeName)
	buf.Comment(fmt.Sprintf("%s configures an existing %s with the passed in options set", withFuncName, c.StructName))
	buf.Func().Id(withFuncName).Params(
		jen.Id(c.ReceiverId).Op("*").Add(c.StructRef...), jen.Id("opts").Op("...").Id(c.OptTypeName),
	).Op("*").Add(c.StructRef...).BlockFunc(applyOptions(c.ReceiverId))
}

func writeWithOptions(buf *jen.File, c Config) {
	withFuncName := "WithOptions"
	buf.Comment(fmt.Sprintf("%s configures the receiver %s with the passed in options set", withFuncName, c.StructName))
	buf.Func().Params(jen.Id(c.ReceiverId).Op("*").Id(c.StructName)).Id(withFuncName).
		Params(jen.Id("opts").Op("...").Id(c.OptTypeName)).Op("*").Add(c.StructRef...).
		BlockFunc(applyOptions(c.ReceiverId))
}

func applyOptions(receiverId string) func(grp *jen.Group) {
	return func(grp *jen.Group) {
		grp.For(jen.Id("_").Op(",").Id("o").Op(":=").Op("range").Id("opts")).Block(
			jen.Id("o").Params(jen.Id(receiverId)),
		)
		grp.Return(jen.Id(receiverId))
	}
}

var genericTypeRegex = regexp.MustCompile("[A-Za-z0-9_]+\\.[A-Za-z0-9_]+\\[(.*)\\]")

// genericFromType provides a means to extract the generic type information
// This returns the type package as first argument, and the unqualified type name as second argument
// FIXME replace with whatever comes out of https://github.com/golang/go/issues/54393
func genericFromType(t types.Type) (string, string) {
	typeName := t.String()
	match := genericTypeRegex.FindStringSubmatch(typeName)
	if len(match) == 2 {
		idx := strings.LastIndex(match[1], ".")
		name := match[1][idx+1:]
		packageName := match[1][:idx]

		return packageName, name
	}
	return "", ""
}

func writeAllWithOptFuncs(buf *jen.File, st *types.Struct, outdir string, c Config) {
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Anonymous() {
			continue
		}

		// don't write options for unexported fields unless the target is the same package
		if !f.Exported() && outdir != f.Pkg().Path() {
			continue
		}

		// build a type specifier based on the field type
		typeRef := typeSpecForType(f.Type(), c)

		switch f.Type().Underlying().(type) {
		case *types.Array, *types.Slice:
			writeSliceWithOpt(buf, f, typeRef, c)
			writeSliceSetOpt(buf, f, typeRef, c)
		case *types.Map:
			writeMapWithOpt(buf, f, typeRef, c)
			writeMapSetOpt(buf, f, typeRef, c)
		default:
			writeStandardWithOpt(buf, f, typeRef, c)
		}
	}
}

func writeSliceWithOpt(buf *jen.File, f *types.Var, ref []jen.Code, c Config) {
	genericPackage, genericName := genericFromType(f.Type())

	ref = ref[1:] // remove the first element, which should be [] for slice types
	fieldFuncName := fmt.Sprintf("With%s", strings.Title(f.Name()))
	buf.Comment(fmt.Sprintf("%s returns an option that can append %ss to %s.%s", fieldFuncName, strings.Title(f.Name()), c.StructName, f.Name()))
	arg := jen.Id(unexport(f.Name())).Add(ref...)
	if genericName != "" {
		arg = arg.Types(jen.Qual(genericPackage, genericName))
	}

	buf.Func().Id(fieldFuncName).Params(arg).Id(c.OptTypeName).BlockFunc(func(grp *jen.Group) {
		grp.Return(
			jen.Func().Params(jen.Id(c.ReceiverId).Op("*").Add(c.StructRef...)).BlockFunc(func(grp2 *jen.Group) {
				grp2.Id(c.ReceiverId).Op(".").Id(f.Name()).Op("=").Append(jen.Id(c.ReceiverId).Op(".").Id(f.Name()), jen.Id(unexport(f.Name())))
			}),
		)
	})
}

func writeSliceSetOpt(buf *jen.File, f *types.Var, ref []jen.Code, c Config) {
	genericPackage, genericName := genericFromType(f.Type())

	fieldFuncName := fmt.Sprintf("Set%s", strings.Title(f.Name()))
	buf.Comment(fmt.Sprintf("%s returns an option that can set %s on a %s", fieldFuncName, strings.Title(f.Name()), c.StructName))

	param := jen.Id(unexport(f.Name())).Add(ref...)
	if genericName != "" {
		param = param.Types(jen.Qual(genericPackage, genericName))
	}
	buf.Func().Id(fieldFuncName).Params(param).Id(c.OptTypeName).BlockFunc(func(grp *jen.Group) {
		grp.Return(
			jen.Func().Params(jen.Id(c.ReceiverId).Op("*").Add(c.StructRef...)).BlockFunc(func(grp2 *jen.Group) {
				grp2.Id(c.ReceiverId).Op(".").Id(f.Name()).Op("=").Id(unexport(f.Name()))
			}),
		)
	})
}

func writeMapWithOpt(buf *jen.File, f *types.Var, ref []jen.Code, c Config) {
	mapType := f.Type()
	for {
		t, ok := mapType.(*types.Map)
		mapType = t
		if ok {
			break
		}
	}
	m := mapType.(*types.Map)
	fieldFuncName := fmt.Sprintf("With%s", strings.Title(f.Name()))
	buf.Comment(fmt.Sprintf("%s returns an option that can append %ss to %s.%s", fieldFuncName, strings.Title(f.Name()), c.StructName, f.Name()))
	buf.Func().Id(fieldFuncName).Params(
		jen.Id("key").Id(m.Key().String()),
		jen.Id("value").Id(m.Elem().String()),
	).Id(c.OptTypeName).BlockFunc(func(grp *jen.Group) {
		grp.Return(
			jen.Func().Params(jen.Id(c.ReceiverId).Op("*").Add(c.StructRef...)).BlockFunc(func(grp2 *jen.Group) {
				grp2.Id(c.ReceiverId).Op(".").Id(f.Name()).Index(jen.Id("key")).Op("=").Id("value")
			}),
		)
	})
}

func writeMapSetOpt(buf *jen.File, f *types.Var, ref []jen.Code, c Config) {
	fieldFuncName := fmt.Sprintf("Set%s", strings.Title(f.Name()))
	buf.Comment(fmt.Sprintf("%s returns an option that can set %s on a %s", fieldFuncName, strings.Title(f.Name()), c.StructName))
	buf.Func().Id(fieldFuncName).Params(
		jen.Id(unexport(f.Name())).Add(ref...),
	).Id(c.OptTypeName).BlockFunc(func(grp *jen.Group) {
		grp.Return(
			jen.Func().Params(jen.Id(c.ReceiverId).Op("*").Add(c.StructRef...)).BlockFunc(func(grp2 *jen.Group) {
				grp2.Id(c.ReceiverId).Op(".").Id(f.Name()).Op("=").Id(unexport(f.Name()))
			}),
		)
	})
}

func writeStandardWithOpt(buf *jen.File, f *types.Var, ref []jen.Code, c Config) {
	fieldFuncName := fmt.Sprintf("With%s", strings.Title(f.Name()))
	buf.Comment(fmt.Sprintf("%s returns an option that can set %s on a %s", fieldFuncName, strings.Title(f.Name()), c.StructName))
	buf.Func().Id(fieldFuncName).Params(
		jen.Id(unexport(f.Name())).Add(ref...),
	).Id(c.OptTypeName).BlockFunc(func(grp *jen.Group) {
		grp.Return(
			jen.Func().Params(jen.Id(c.ReceiverId).Op("*").Add(c.StructRef...)).BlockFunc(func(grp2 *jen.Group) {
				grp2.Id(c.ReceiverId).Op(".").Id(f.Name()).Op("=").Id(unexport(f.Name()))
			}),
		)
	})
}

func typeSpecForType(in types.Type, c Config) (ref []jen.Code) {
	ref = make([]jen.Code, 0)
	current := in

	depth := 0
	for {
		depth++
		switch t := current.(type) {
		case *types.Array:
			ref = append(ref, jen.Index())
			current = t.Elem()
		case *types.Slice:
			ref = append(ref, jen.Index())
			current = t.Elem()
		case *types.Pointer:
			ref = append(ref, jen.Op("*"))
			current = t.Elem()
		case *types.Named:
			if t.Obj().Pkg().Path() == c.PkgPath {
				ref = append(ref, jen.Id(t.Obj().Name()))
			} else {
				ref = append(ref, jen.Qual(t.Obj().Pkg().Path(), t.Obj().Name()))
			}
			return
		case *types.Basic:
			ref = append(ref, jen.Id(t.Name()))
			return
		case *types.Struct:
			ref = append(ref, jen.Struct())
			return
		case *types.Map:
			ref = append(ref, jen.Map(jen.Id(t.Key().String())).Id(t.Elem().String()))
			return
		default:
			if depth > 10 {
				panic(fmt.Sprintf("optgen doesn't know how to generate for type %s, please file a bug", in.String()))
			}
		}
	}
}

func unexport(s string) string {
	if len(s) == 0 {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}
