// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package internal_gengo is internal to the protobuf module.
package internal_gengo

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/internal/editionssupport"
	"google.golang.org/protobuf/internal/encoding/tag"
	"google.golang.org/protobuf/internal/filedesc"
	"google.golang.org/protobuf/internal/genid"
	"google.golang.org/protobuf/internal/version"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/runtime/protoimpl"

	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/gofeaturespb"
	"google.golang.org/protobuf/types/pluginpb"
)

// SupportedFeatures reports the set of supported protobuf language features.
var SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL | pluginpb.CodeGeneratorResponse_FEATURE_SUPPORTS_EDITIONS)

var SupportedEditionsMinimum = editionssupport.Minimum
var SupportedEditionsMaximum = editionssupport.Maximum

// GenerateVersionMarkers specifies whether to generate version markers.
var GenerateVersionMarkers = true

// Standard library dependencies.
const (
	base64Package  = protogen.GoImportPath("encoding/base64")
	jsonPackage    = protogen.GoImportPath("encoding/json")
	mathPackage    = protogen.GoImportPath("math")
	reflectPackage = protogen.GoImportPath("reflect")
	sortPackage    = protogen.GoImportPath("sort")
	stringsPackage = protogen.GoImportPath("strings")
	syncPackage    = protogen.GoImportPath("sync")
	timePackage    = protogen.GoImportPath("time")
	utf8Package    = protogen.GoImportPath("unicode/utf8")
	unsafePackage  = protogen.GoImportPath("unsafe")
)

// Protobuf library dependencies.
//
// These are declared as an interface type so that they can be more easily
// patched to support unique build environments that impose restrictions
// on the dependencies of generated source code.
var (
	protoPackage         goImportPath = protogen.GoImportPath("google.golang.org/protobuf/proto")
	protoifacePackage    goImportPath = protogen.GoImportPath("google.golang.org/protobuf/runtime/protoiface")
	protoimplPackage     goImportPath = protogen.GoImportPath("google.golang.org/protobuf/runtime/protoimpl")
	protojsonPackage     goImportPath = protogen.GoImportPath("google.golang.org/protobuf/encoding/protojson")
	protoreflectPackage  goImportPath = protogen.GoImportPath("google.golang.org/protobuf/reflect/protoreflect")
	protoregistryPackage goImportPath = protogen.GoImportPath("google.golang.org/protobuf/reflect/protoregistry")
)

type goImportPath interface {
	String() string
	Ident(string) protogen.GoIdent
}

func setToOpaque(msg *protogen.Message) {
	msg.APILevel = gofeaturespb.GoFeatures_API_OPAQUE
	for _, nested := range msg.Messages {
		nested.APILevel = gofeaturespb.GoFeatures_API_OPAQUE
		setToOpaque(nested)
	}
}

// GenerateFile generates the contents of a .pb.go file.
//
// With the Hybrid API, multiple files are generated (_protoopaque.pb.go variant),
// but only the first file (regular, not a variant) is returned.
func GenerateFile(gen *protogen.Plugin, file *protogen.File) *protogen.GeneratedFile {
	return generateFiles(gen, file)[0]
}

func generateFiles(gen *protogen.Plugin, file *protogen.File) []*protogen.GeneratedFile {
	f := newFileInfo(file)
	generated := []*protogen.GeneratedFile{
		generateOneFile(gen, file, f, ""),
	}
	if f.APILevel == gofeaturespb.GoFeatures_API_HYBRID {
		// Update all APILevel fields to OPAQUE
		f.APILevel = gofeaturespb.GoFeatures_API_OPAQUE
		for _, msg := range f.Messages {
			setToOpaque(msg)
		}
		generated = append(generated, generateOneFile(gen, file, f, "_protoopaque"))
	}
	return generated
}

func generateOneFile(gen *protogen.Plugin, file *protogen.File, f *fileInfo, variant string) *protogen.GeneratedFile {
	filename := file.GeneratedFilenamePrefix + variant + ".pb.go"
	g := gen.NewGeneratedFile(filename, file.GoImportPath)

	var packageDoc protogen.Comments
	if !gen.InternalStripForEditionsDiff() {
		genStandaloneComments(g, f, int32(genid.FileDescriptorProto_Syntax_field_number))
		genGeneratedHeader(gen, g, f)
		genStandaloneComments(g, f, int32(genid.FileDescriptorProto_Package_field_number))

		packageDoc = genPackageKnownComment(f)
	}
	if variant == "_protoopaque" {
		g.P("//go:build protoopaque")
	} else if f.APILevel == gofeaturespb.GoFeatures_API_HYBRID {
		g.P("//go:build !protoopaque")
	}
	g.P(packageDoc, "package ", f.GoPackageName)
	g.P()

	// Emit a static check that enforces a minimum version of the proto package.
	if GenerateVersionMarkers {
		g.P("const (")
		g.P("// Verify that this generated code is sufficiently up-to-date.")
		g.P("_ = ", protoimplPackage.Ident("EnforceVersion"), "(", protoimpl.GenVersion, " - ", protoimplPackage.Ident("MinVersion"), ")")
		g.P("// Verify that runtime/protoimpl is sufficiently up-to-date.")
		g.P("_ = ", protoimplPackage.Ident("EnforceVersion"), "(", protoimplPackage.Ident("MaxVersion"), " - ", protoimpl.GenVersion, ")")
		g.P(")")
		g.P()
	}

	for i, imps := 0, f.Desc.Imports(); i < imps.Len(); i++ {
		genImport(gen, g, f, imps.Get(i))
	}
	for _, enum := range f.allEnums {
		genEnum(g, f, enum)
	}
	for _, message := range f.allMessages {
		genMessage(g, f, message)
	}
	genExtensions(g, f)

	// The descriptor contains a lot of information about the syntax which is
	// quite different between the proto2/3 version of a file and the equivalent
	// editions version. For example, when a proto3 file is translated from
	// proto3 to editions every field in that file that is marked optional in
	// proto3 will have a features.field_presence option set.
	// Another problem is that the descriptor contains implementation details
	// that are not relevant for the semantic. For example, proto3 optional
	// fields are implemented as oneof fields with one case. The descriptor
	// contains different information about oneofs. If the file is translated
	// to editions it no longer is treated as a oneof with one case and thus
	// none of the oneof specific information is generated.
	// To be able to compare the descriptor before and after translation of the
	// associated proto file, we would need to trim many parts. This would lead
	// to a brittle implementation in case the translation ever changes.
	if !g.InternalStripForEditionsDiff() {
		genReflectFileDescriptor(gen, g, f)
	}

	return g
}

// genStandaloneComments prints all leading comments for a FileDescriptorProto
// location identified by the field number n.
func genStandaloneComments(g *protogen.GeneratedFile, f *fileInfo, n int32) {
	loc := f.Desc.SourceLocations().ByPath(protoreflect.SourcePath{n})
	for _, s := range loc.LeadingDetachedComments {
		g.P(protogen.Comments(s))
		g.P()
	}
	if s := loc.LeadingComments; s != "" {
		g.P(protogen.Comments(s))
		g.P()
	}
}

func genGeneratedHeader(gen *protogen.Plugin, g *protogen.GeneratedFile, f *fileInfo) {
	g.P("// Code generated by protoc-gen-go. DO NOT EDIT.")

	if GenerateVersionMarkers {
		g.P("// versions:")
		protocGenGoVersion := version.String()
		protocVersion := "(unknown)"
		if v := gen.Request.GetCompilerVersion(); v != nil {
			protocVersion = fmt.Sprintf("v%v.%v.%v", v.GetMajor(), v.GetMinor(), v.GetPatch())
			if s := v.GetSuffix(); s != "" {
				protocVersion += "-" + s
			}
		}
		g.P("// \tprotoc-gen-go ", protocGenGoVersion)
		g.P("// \tprotoc        ", protocVersion)
	}

	if f.Proto.GetOptions().GetDeprecated() {
		g.P("// ", f.Desc.Path(), " is a deprecated file.")
	} else {
		g.P("// source: ", f.Desc.Path())
	}
	g.P()
}

func genImport(gen *protogen.Plugin, g *protogen.GeneratedFile, f *fileInfo, imp protoreflect.FileImport) {
	impFile, ok := gen.FilesByPath[imp.Path()]
	if !ok {
		return
	}
	if impFile.GoImportPath == f.GoImportPath {
		// Don't generate imports or aliases for types in the same Go package.
		return
	}
	// Generate imports for all dependencies, even if they are not
	// referenced, because other code and tools depend on having the
	// full transitive closure of protocol buffer types in the binary.
	g.Import(impFile.GoImportPath)
	if !imp.IsPublic {
		return
	}

	// Generate public imports by generating the imported file, parsing it,
	// and extracting every symbol that should receive a forwarding declaration.
	impGens := generateFiles(gen, impFile)
	for _, impGen := range impGens {
		impGen.Skip()
	}
	b, err := impGens[0].Content()
	if err != nil {
		gen.Error(err)
		return
	}
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, "", b, parser.ParseComments)
	if err != nil {
		gen.Error(err)
		return
	}
	genForward := func(tok token.Token, name string, expr ast.Expr) {
		// Don't import unexported symbols.
		r, _ := utf8.DecodeRuneInString(name)
		if !unicode.IsUpper(r) {
			return
		}
		// Don't import the FileDescriptor.
		if name == impFile.GoDescriptorIdent.GoName {
			return
		}
		// Don't import decls referencing a symbol defined in another package.
		// i.e., don't import decls which are themselves public imports:
		//
		//	type T = somepackage.T
		if _, ok := expr.(*ast.SelectorExpr); ok {
			return
		}
		g.P(tok, " ", name, " = ", impFile.GoImportPath.Ident(name))
	}
	g.P("// Symbols defined in public import of ", imp.Path(), ".")
	g.P()
	for _, decl := range astFile.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					genForward(decl.Tok, spec.Name.Name, spec.Type)
				case *ast.ValueSpec:
					for i, name := range spec.Names {
						var expr ast.Expr
						if i < len(spec.Values) {
							expr = spec.Values[i]
						}
						genForward(decl.Tok, name.Name, expr)
					}
				case *ast.ImportSpec:
				default:
					panic(fmt.Sprintf("can't generate forward for spec type %T", spec))
				}
			}
		}
	}
	g.P()
}

func genEnum(g *protogen.GeneratedFile, f *fileInfo, e *enumInfo) {
	// Enum type declaration.
	g.AnnotateSymbol(e.GoIdent.GoName, protogen.Annotation{Location: e.Location})
	leadingComments := appendDeprecationSuffix(e.Comments.Leading,
		e.Desc.ParentFile(),
		e.Desc.Options().(*descriptorpb.EnumOptions).GetDeprecated())
	g.P(leadingComments,
		"type ", e.GoIdent, " int32")

	// Enum value constants.
	g.P("const (")
	anyOldName := false
	for _, value := range e.Values {
		g.AnnotateSymbol(value.GoIdent.GoName, protogen.Annotation{Location: value.Location})
		leadingComments := appendDeprecationSuffix(value.Comments.Leading,
			value.Desc.ParentFile(),
			value.Desc.Options().(*descriptorpb.EnumValueOptions).GetDeprecated())
		g.P(leadingComments,
			value.GoIdent, " ", e.GoIdent, " = ", value.Desc.Number(),
			trailingComment(value.Comments.Trailing))

		if value.PrefixedAlias.GoName != "" &&
			value.PrefixedAlias.GoName != value.GoIdent.GoName {
			anyOldName = true
		}
	}
	g.P(")")
	g.P()
	if anyOldName {
		g.P("// Old (prefixed) names for ", e.GoIdent, " enum values.")
		g.P("const (")
		for _, value := range e.Values {
			if value.PrefixedAlias.GoName != "" &&
				value.PrefixedAlias.GoName != value.GoIdent.GoName {
				g.P(value.PrefixedAlias, " ", e.GoIdent, " = ", value.GoIdent)
			}
		}
		g.P(")")
		g.P()
	}

	// Enum value maps.
	g.P("// Enum value maps for ", e.GoIdent, ".")
	g.P("var (")
	g.P(e.GoIdent.GoName+"_name", " = map[int32]string{")
	for _, value := range e.Values {
		duplicate := ""
		if value.Desc != e.Desc.Values().ByNumber(value.Desc.Number()) {
			duplicate = "// Duplicate value: "
		}
		g.P(duplicate, value.Desc.Number(), ": ", strconv.Quote(string(value.Desc.Name())), ",")
	}
	g.P("}")
	g.P(e.GoIdent.GoName+"_value", " = map[string]int32{")
	for _, value := range e.Values {
		g.P(strconv.Quote(string(value.Desc.Name())), ": ", value.Desc.Number(), ",")
	}
	g.P("}")
	g.P(")")
	g.P()

	// Enum method.
	//
	// NOTE: A pointer value is needed to represent presence in proto2.
	// Since a proto2 message can reference a proto3 enum, it is useful to
	// always generate this method (even on proto3 enums) to support that case.
	g.P("func (x ", e.GoIdent, ") Enum() *", e.GoIdent, " {")
	g.P("p := new(", e.GoIdent, ")")
	g.P("*p = x")
	g.P("return p")
	g.P("}")
	g.P()

	// String method.
	g.P("func (x ", e.GoIdent, ") String() string {")
	g.P("return ", protoimplPackage.Ident("X"), ".EnumStringOf(x.Descriptor(), ", protoreflectPackage.Ident("EnumNumber"), "(x))")
	g.P("}")
	g.P()

	genEnumReflectMethods(g, f, e)

	// UnmarshalJSON method.
	needsUnmarshalJSONMethod := false
	if fde, ok := e.Desc.(*filedesc.Enum); ok {
		needsUnmarshalJSONMethod = fde.L1.EditionFeatures.GenerateLegacyUnmarshalJSON
	}
	if e.genJSONMethod && needsUnmarshalJSONMethod {
		g.P("// Deprecated: Do not use.")
		g.P("func (x *", e.GoIdent, ") UnmarshalJSON(b []byte) error {")
		g.P("num, err := ", protoimplPackage.Ident("X"), ".UnmarshalJSONEnum(x.Descriptor(), b)")
		g.P("if err != nil {")
		g.P("return err")
		g.P("}")
		g.P("*x = ", e.GoIdent, "(num)")
		g.P("return nil")
		g.P("}")
		g.P()
	}

	// EnumDescriptor method.
	if e.genRawDescMethod {
		var indexes []string
		for i := 1; i < len(e.Location.Path); i += 2 {
			indexes = append(indexes, strconv.Itoa(int(e.Location.Path[i])))
		}
		g.P("// Deprecated: Use ", e.GoIdent, ".Descriptor instead.")
		g.P("func (", e.GoIdent, ") EnumDescriptor() ([]byte, []int) {")
		g.P("return ", rawDescVarName(f), "GZIP(), []int{", strings.Join(indexes, ","), "}")
		g.P("}")
		g.P()
		f.needRawDesc = true
	}
}

func genMessage(g *protogen.GeneratedFile, f *fileInfo, m *messageInfo) {
	if m.Desc.IsMapEntry() {
		return
	}
	if opaqueGenMessageHook(g, f, m) {
		return
	}

	// Message type declaration.
	g.AnnotateSymbol(m.GoIdent.GoName, protogen.Annotation{Location: m.Location})
	leadingComments := appendDeprecationSuffix(m.Comments.Leading,
		m.Desc.ParentFile(),
		m.Desc.Options().(*descriptorpb.MessageOptions).GetDeprecated())
	g.P(leadingComments,
		"type ", m.GoIdent, " struct {")
	genMessageFields(g, f, m)
	g.P("}")
	g.P()

	genMessageKnownFunctions(g, f, m)
	genMessageDefaultDecls(g, f, m)
	genMessageMethods(g, f, m)
	genMessageOneofWrapperTypes(g, f, m)
}

func genMessageFields(g *protogen.GeneratedFile, f *fileInfo, m *messageInfo) {
	sf := f.allMessageFieldsByPtr[m]
	genMessageInternalFields(g, f, m, sf)
	for _, field := range m.Fields {
		genMessageField(g, f, m, field, sf)
	}
}

func genMessageInternalFields(g *protogen.GeneratedFile, f *fileInfo, m *messageInfo, sf *structFields) {
	g.P(genid.State_goname, " ", protoimplPackage.Ident("MessageState"))
	sf.append(genid.State_goname)
	g.P(genid.SizeCache_goname, " ", protoimplPackage.Ident("SizeCache"))
	sf.append(genid.SizeCache_goname)
	g.P(genid.UnknownFields_goname, " ", protoimplPackage.Ident("UnknownFields"))
	sf.append(genid.UnknownFields_goname)
	if m.Desc.ExtensionRanges().Len() > 0 {
		g.P(genid.ExtensionFields_goname, " ", protoimplPackage.Ident("ExtensionFields"))
		sf.append(genid.ExtensionFields_goname)
	}
	if sf.count > 0 {
		g.P()
	}
}

func genMessageField(g *protogen.GeneratedFile, f *fileInfo, m *messageInfo, field *protogen.Field, sf *structFields) {
	if oneof := field.Oneof; oneof != nil && !oneof.Desc.IsSynthetic() {
		// It would be a bit simpler to iterate over the oneofs below,
		// but generating the field here keeps the contents of the Go
		// struct in the same order as the contents of the source
		// .proto file.
		if oneof.Fields[0] != field {
			return // only generate for first appearance
		}

		tags := structTags{
			{"protobuf_oneof", string(oneof.Desc.Name())},
		}
		if m.isTracked {
			tags = append(tags, gotrackTags...)
		}

		g.AnnotateSymbol(m.GoIdent.GoName+"."+oneof.GoName, protogen.Annotation{Location: oneof.Location})
		leadingComments := oneof.Comments.Leading
		if leadingComments != "" {
			leadingComments += "\n"
		}
		ss := []string{fmt.Sprintf(" Types that are assignable to %s:\n", oneof.GoName)}
		for _, field := range oneof.Fields {
			ss = append(ss, "\t*"+field.GoIdent.GoName+"\n")
		}
		leadingComments += protogen.Comments(strings.Join(ss, ""))
		g.P(leadingComments,
			oneof.GoName, " ", oneofInterfaceName(oneof), tags)
		sf.append(oneof.GoName)
		return
	}
	goType, pointer := fieldGoType(g, f, field)
	if pointer {
		goType = "*" + goType
	}
	tags := structTags{
		{"protobuf", fieldProtobufTagValue(field)},
		{"json", fieldJSONTagValue(field)},
	}
	if field.Desc.IsMap() {
		key := field.Message.Fields[0]
		val := field.Message.Fields[1]
		tags = append(tags, structTags{
			{"protobuf_key", fieldProtobufTagValue(key)},
			{"protobuf_val", fieldProtobufTagValue(val)},
		}...)
	}
	if m.isTracked {
		tags = append(tags, gotrackTags...)
	}

	name := field.GoName
	g.AnnotateSymbol(m.GoIdent.GoName+"."+name, protogen.Annotation{Location: field.Location})
	leadingComments := appendDeprecationSuffix(field.Comments.Leading,
		field.Desc.ParentFile(),
		field.Desc.Options().(*descriptorpb.FieldOptions).GetDeprecated())
	g.P(leadingComments,
		name, " ", goType, tags,
		trailingComment(field.Comments.Trailing))
	sf.append(field.GoName)
}

// genMessageDefaultDecls generates consts and vars holding the default
// values of fields.
func genMessageDefaultDecls(g *protogen.GeneratedFile, f *fileInfo, m *messageInfo) {
	var consts, vars []string
	for _, field := range m.Fields {
		if !field.Desc.HasDefault() {
			continue
		}
		name := "Default_" + m.GoIdent.GoName + "_" + field.GoName
		goType, _ := fieldGoType(g, f, field)
		defVal := field.Desc.Default()
		switch field.Desc.Kind() {
		case protoreflect.StringKind:
			consts = append(consts, fmt.Sprintf("%s = %s(%q)", name, goType, defVal.String()))
		case protoreflect.BytesKind:
			vars = append(vars, fmt.Sprintf("%s = %s(%q)", name, goType, defVal.Bytes()))
		case protoreflect.EnumKind:
			idx := field.Desc.DefaultEnumValue().Index()
			val := field.Enum.Values[idx]
			if val.GoIdent.GoImportPath == f.GoImportPath {
				consts = append(consts, fmt.Sprintf("%s = %s", name, g.QualifiedGoIdent(val.GoIdent)))
			} else {
				// If the enum value is declared in a different Go package,
				// reference it by number since the name may not be correct.
				// See https://github.com/golang/protobuf/issues/513.
				consts = append(consts, fmt.Sprintf("%s = %s(%d) // %s",
					name, g.QualifiedGoIdent(field.Enum.GoIdent), val.Desc.Number(), g.QualifiedGoIdent(val.GoIdent)))
			}
		case protoreflect.FloatKind, protoreflect.DoubleKind:
			if f := defVal.Float(); math.IsNaN(f) || math.IsInf(f, 0) {
				var fn, arg string
				switch f := defVal.Float(); {
				case math.IsInf(f, -1):
					fn, arg = g.QualifiedGoIdent(mathPackage.Ident("Inf")), "-1"
				case math.IsInf(f, +1):
					fn, arg = g.QualifiedGoIdent(mathPackage.Ident("Inf")), "+1"
				case math.IsNaN(f):
					fn, arg = g.QualifiedGoIdent(mathPackage.Ident("NaN")), ""
				}
				vars = append(vars, fmt.Sprintf("%s = %s(%s(%s))", name, goType, fn, arg))
			} else {
				consts = append(consts, fmt.Sprintf("%s = %s(%v)", name, goType, f))
			}
		default:
			consts = append(consts, fmt.Sprintf("%s = %s(%v)", name, goType, defVal.Interface()))
		}
	}
	if len(consts) > 0 {
		g.P("// Default values for ", m.GoIdent, " fields.")
		g.P("const (")
		for _, s := range consts {
			g.P(s)
		}
		g.P(")")
	}
	if len(vars) > 0 {
		g.P("// Default values for ", m.GoIdent, " fields.")
		g.P("var (")
		for _, s := range vars {
			g.P(s)
		}
		g.P(")")
	}
	g.P()
}

func genMessageMethods(g *protogen.GeneratedFile, f *fileInfo, m *messageInfo) {
	genMessageBaseMethods(g, f, m)
	genMessageGetterMethods(g, f, m)
}

func genMessageBaseMethods(g *protogen.GeneratedFile, f *fileInfo, m *messageInfo) {
	// Reset method.
	g.P("func (x *", m.GoIdent, ") Reset() {")
	g.P("*x = ", m.GoIdent, "{}")
	g.P("mi := &", messageTypesVarName(f), "[", f.allMessagesByPtr[m], "]")
	g.P("ms := ", protoimplPackage.Ident("X"), ".MessageStateOf(", protoimplPackage.Ident("Pointer"), "(x))")
	g.P("ms.StoreMessageInfo(mi)")
	g.P("}")
	g.P()

	// String method.
	g.P("func (x *", m.GoIdent, ") String() string {")
	g.P("return ", protoimplPackage.Ident("X"), ".MessageStringOf(x)")
	g.P("}")
	g.P()

	// ProtoMessage method.
	g.P("func (*", m.GoIdent, ") ProtoMessage() {}")
	g.P()

	// ProtoReflect method.
	genMessageReflectMethods(g, f, m)

	// Descriptor method.
	if m.genRawDescMethod {
		var indexes []string
		for i := 1; i < len(m.Location.Path); i += 2 {
			indexes = append(indexes, strconv.Itoa(int(m.Location.Path[i])))
		}
		g.P("// Deprecated: Use ", m.GoIdent, ".ProtoReflect.Descriptor instead.")
		g.P("func (*", m.GoIdent, ") Descriptor() ([]byte, []int) {")
		g.P("return ", rawDescVarName(f), "GZIP(), []int{", strings.Join(indexes, ","), "}")
		g.P("}")
		g.P()
		f.needRawDesc = true
	}
}

func genMessageGetterMethods(g *protogen.GeneratedFile, f *fileInfo, m *messageInfo) {
	for _, field := range m.Fields {
		genNoInterfacePragma(g, m.isTracked)

		// Getter for parent oneof.
		if oneof := field.Oneof; oneof != nil && oneof.Fields[0] == field && !oneof.Desc.IsSynthetic() {
			g.AnnotateSymbol(m.GoIdent.GoName+".Get"+oneof.GoName, protogen.Annotation{Location: oneof.Location})
			g.P("func (m *", m.GoIdent.GoName, ") Get", oneof.GoName, "() ", oneofInterfaceName(oneof), " {")
			g.P("if m != nil {")
			g.P("return m.", oneof.GoName)
			g.P("}")
			g.P("return nil")
			g.P("}")
			g.P()
		}

		// Getter for message field.
		goType, pointer := fieldGoType(g, f, field)
		defaultValue := fieldDefaultValue(g, f, m, field)
		g.AnnotateSymbol(m.GoIdent.GoName+".Get"+field.GoName, protogen.Annotation{Location: field.Location})
		leadingComments := appendDeprecationSuffix("",
			field.Desc.ParentFile(),
			field.Desc.Options().(*descriptorpb.FieldOptions).GetDeprecated())
		switch {
		case field.Oneof != nil && !field.Oneof.Desc.IsSynthetic():
			g.P(leadingComments, "func (x *", m.GoIdent, ") Get", field.GoName, "() ", goType, " {")
			g.P("if x, ok := x.Get", field.Oneof.GoName, "().(*", field.GoIdent, "); ok {")
			g.P("return x.", field.GoName)
			g.P("}")
			g.P("return ", defaultValue)
			g.P("}")
		default:
			g.P(leadingComments, "func (x *", m.GoIdent, ") Get", field.GoName, "() ", goType, " {")
			if !field.Desc.HasPresence() || defaultValue == "nil" {
				g.P("if x != nil {")
			} else {
				g.P("if x != nil && x.", field.GoName, " != nil {")
			}
			star := ""
			if pointer {
				star = "*"
			}
			g.P("return ", star, " x.", field.GoName)
			g.P("}")
			g.P("return ", defaultValue)
			g.P("}")
		}
		g.P()
	}
}

// fieldGoType returns the Go type used for a field.
//
// If it returns pointer=true, the struct field is a pointer to the type.
func fieldGoType(g *protogen.GeneratedFile, f *fileInfo, field *protogen.Field) (goType string, pointer bool) {
	pointer = field.Desc.HasPresence()
	switch field.Desc.Kind() {
	case protoreflect.BoolKind:
		goType = "bool"
	case protoreflect.EnumKind:
		goType = g.QualifiedGoIdent(field.Enum.GoIdent)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		goType = "int32"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		goType = "uint32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		goType = "int64"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		goType = "uint64"
	case protoreflect.FloatKind:
		goType = "float32"
	case protoreflect.DoubleKind:
		goType = "float64"
	case protoreflect.StringKind:
		goType = "string"
	case protoreflect.BytesKind:
		goType = "[]byte"
		pointer = false // rely on nullability of slices for presence
	case protoreflect.MessageKind, protoreflect.GroupKind:
		goType = "*" + g.QualifiedGoIdent(field.Message.GoIdent)
		pointer = false // pointer captured as part of the type
	}
	switch {
	case field.Desc.IsList():
		return "[]" + goType, false
	case field.Desc.IsMap():
		keyType, _ := fieldGoType(g, f, field.Message.Fields[0])
		valType, _ := fieldGoType(g, f, field.Message.Fields[1])
		return fmt.Sprintf("map[%v]%v", keyType, valType), false
	}
	return goType, pointer
}

func fieldProtobufTagValue(field *protogen.Field) string {
	var enumName string
	if field.Desc.Kind() == protoreflect.EnumKind {
		enumName = protoimpl.X.LegacyEnumName(field.Enum.Desc)
	}
	return tag.Marshal(field.Desc, enumName)
}

func fieldDefaultValue(g *protogen.GeneratedFile, f *fileInfo, m *messageInfo, field *protogen.Field) string {
	if field.Desc.IsList() {
		return "nil"
	}
	if field.Desc.HasDefault() {
		defVarName := "Default_" + m.GoIdent.GoName + "_" + field.GoName
		if field.Desc.Kind() == protoreflect.BytesKind {
			return "append([]byte(nil), " + defVarName + "...)"
		}
		return defVarName
	}
	switch field.Desc.Kind() {
	case protoreflect.BoolKind:
		return "false"
	case protoreflect.StringKind:
		return `""`
	case protoreflect.MessageKind, protoreflect.GroupKind, protoreflect.BytesKind:
		return "nil"
	case protoreflect.EnumKind:
		val := field.Enum.Values[0]
		if val.GoIdent.GoImportPath == f.GoImportPath {
			return g.QualifiedGoIdent(val.GoIdent)
		} else {
			// If the enum value is declared in a different Go package,
			// reference it by number since the name may not be correct.
			// See https://github.com/golang/protobuf/issues/513.
			return g.QualifiedGoIdent(field.Enum.GoIdent) + "(" + strconv.FormatInt(int64(val.Desc.Number()), 10) + ")"
		}
	default:
		return "0"
	}
}

func fieldJSONTagValue(field *protogen.Field) string {
	if field.Desc.JSONName() == "-" {
		return "-"
	} else if field.Desc.JSONName() == "MUST" {
		return string(field.Desc.Name())
	}
	return string(field.Desc.Name()) + ",omitempty"
}

func fieldFORMTagValue(field *protogen.Field) string {
	return string(field.Desc.Name())
}

func fieldURITagValue(field *protogen.Field) string {
	return string(field.Desc.Name())
}

func genExtensions(g *protogen.GeneratedFile, f *fileInfo) {
	if len(f.allExtensions) == 0 {
		return
	}

	g.P("var ", extensionTypesVarName(f), " = []", protoimplPackage.Ident("ExtensionInfo"), "{")
	for _, x := range f.allExtensions {
		g.P("{")
		g.P("ExtendedType: (*", x.Extendee.GoIdent, ")(nil),")
		goType, pointer := fieldGoType(g, f, x.Extension)
		if pointer {
			goType = "*" + goType
		}
		g.P("ExtensionType: (", goType, ")(nil),")
		g.P("Field: ", x.Desc.Number(), ",")
		g.P("Name: ", strconv.Quote(string(x.Desc.FullName())), ",")
		g.P("Tag: ", strconv.Quote(fieldProtobufTagValue(x.Extension)), ",")
		g.P("Filename: ", strconv.Quote(f.Desc.Path()), ",")
		g.P("},")
	}
	g.P("}")
	g.P()

	// Group extensions by the target message.
	var orderedTargets []protogen.GoIdent
	allExtensionsByTarget := make(map[protogen.GoIdent][]*extensionInfo)
	allExtensionsByPtr := make(map[*extensionInfo]int)
	for i, x := range f.allExtensions {
		target := x.Extendee.GoIdent
		if len(allExtensionsByTarget[target]) == 0 {
			orderedTargets = append(orderedTargets, target)
		}
		allExtensionsByTarget[target] = append(allExtensionsByTarget[target], x)
		allExtensionsByPtr[x] = i
	}
	for _, target := range orderedTargets {
		g.P("// Extension fields to ", target, ".")
		g.P("var (")
		for _, x := range allExtensionsByTarget[target] {
			xd := x.Desc
			typeName := xd.Kind().String()
			switch xd.Kind() {
			case protoreflect.EnumKind:
				typeName = string(xd.Enum().FullName())
			case protoreflect.MessageKind, protoreflect.GroupKind:
				typeName = string(xd.Message().FullName())
			}
			fieldName := string(xd.Name())

			leadingComments := x.Comments.Leading
			if leadingComments != "" {
				leadingComments += "\n"
			}
			leadingComments += protogen.Comments(fmt.Sprintf(" %v %v %v = %v;\n",
				xd.Cardinality(), typeName, fieldName, xd.Number()))
			leadingComments = appendDeprecationSuffix(leadingComments,
				x.Desc.ParentFile(),
				x.Desc.Options().(*descriptorpb.FieldOptions).GetDeprecated())
			g.P(leadingComments,
				"E_", x.GoIdent, " = &", extensionTypesVarName(f), "[", allExtensionsByPtr[x], "]",
				trailingComment(x.Comments.Trailing))
		}
		g.P(")")
		g.P()
	}
}

// genMessageOneofWrapperTypes generates the oneof wrapper types and
// associates the types with the parent message type.
func genMessageOneofWrapperTypes(g *protogen.GeneratedFile, f *fileInfo, m *messageInfo) {
	for _, oneof := range m.Oneofs {
		if oneof.Desc.IsSynthetic() {
			continue
		}
		ifName := oneofInterfaceName(oneof)
		g.P("type ", ifName, " interface {")
		g.P(ifName, "()")
		g.P("}")
		g.P()
		for _, field := range oneof.Fields {
			g.AnnotateSymbol(field.GoIdent.GoName, protogen.Annotation{Location: field.Location})
			g.AnnotateSymbol(field.GoIdent.GoName+"."+field.GoName, protogen.Annotation{Location: field.Location})
			g.P("type ", field.GoIdent, " struct {")
			goType, _ := fieldGoType(g, f, field)
			tags := structTags{
				{"protobuf", fieldProtobufTagValue(field)},
			}
			if m.isTracked {
				tags = append(tags, gotrackTags...)
			}
			leadingComments := appendDeprecationSuffix(field.Comments.Leading,
				field.Desc.ParentFile(),
				field.Desc.Options().(*descriptorpb.FieldOptions).GetDeprecated())
			g.P(leadingComments,
				field.GoName, " ", goType, tags,
				trailingComment(field.Comments.Trailing))
			g.P("}")
			g.P()
		}
		for _, field := range oneof.Fields {
			g.P("func (*", field.GoIdent, ") ", ifName, "() {}")
			g.P()
		}
	}
}

// oneofInterfaceName returns the name of the interface type implemented by
// the oneof field value types.
func oneofInterfaceName(oneof *protogen.Oneof) string {
	return "is" + oneof.GoIdent.GoName
}

// genNoInterfacePragma generates a standalone "nointerface" pragma to
// decorate methods with field-tracking support.
func genNoInterfacePragma(g *protogen.GeneratedFile, tracked bool) {
	if tracked {
		g.P("//go:nointerface")
		g.P()
	}
}

var gotrackTags = structTags{{"go", "track"}}

// structTags is a data structure for build idiomatic Go struct tags.
// Each [2]string is a key-value pair, where value is the unescaped string.
//
// Example: structTags{{"key", "value"}}.String() -> `key:"value"`
type structTags [][2]string

func (tags structTags) String() string {
	if len(tags) == 0 {
		return ""
	}
	var ss []string
	for _, tag := range tags {
		// NOTE: When quoting the value, we need to make sure the backtick
		// character does not appear. Convert all cases to the escaped hex form.
		key := tag[0]
		val := strings.Replace(strconv.Quote(tag[1]), "`", `\x60`, -1)
		ss = append(ss, fmt.Sprintf("%s:%s", key, val))
	}
	return "`" + strings.Join(ss, " ") + "`"
}

// appendDeprecationSuffix optionally appends a deprecation notice as a suffix.
func appendDeprecationSuffix(prefix protogen.Comments, parentFile protoreflect.FileDescriptor, deprecated bool) protogen.Comments {
	fileDeprecated := parentFile.Options().(*descriptorpb.FileOptions).GetDeprecated()
	if !deprecated && !fileDeprecated {
		return prefix
	}
	if prefix != "" {
		prefix += "\n"
	}
	if fileDeprecated {
		return prefix + " Deprecated: The entire proto file " + protogen.Comments(parentFile.Path()) + " is marked as deprecated.\n"
	}
	return prefix + " Deprecated: Marked as deprecated in " + protogen.Comments(parentFile.Path()) + ".\n"
}

// trailingComment is like protogen.Comments, but lacks a trailing newline.
type trailingComment protogen.Comments

func (c trailingComment) String() string {
	s := strings.TrimSuffix(protogen.Comments(c).String(), "\n")
	if strings.Contains(s, "\n") {
		// We don't support multi-lined trailing comments as it is unclear
		// how to best render them in the generated code.
		return ""
	}
	return s
}
