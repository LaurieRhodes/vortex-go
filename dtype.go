package vortex

// DType represents a Vortex logical data type.
// Vortex DTypes are strictly logical — they define the domain of values,
// not how they're physically stored.
type DType struct {
	Kind     DTypeKind
	Nullable bool

	// For Primitive types
	PType PType

	// For Decimal types
	Precision uint8
	Scale     int8

	// For Struct types
	FieldNames []string
	FieldTypes []*DType

	// For List / FixedSizeList types
	ElementType *DType
	FixedSize   uint32 // only for FixedSizeList

	// For Extension types
	ExtensionID string
	StorageDType *DType
	ExtMetadata  []byte
}

// DTypeKind enumerates the logical type categories.
type DTypeKind int

const (
	DTypeNull DTypeKind = iota + 1
	DTypeBool
	DTypePrimitive
	DTypeDecimal
	DTypeUtf8
	DTypeBinary
	DTypeStruct
	DTypeList
	DTypeFixedSizeList
	DTypeExtension
)

// PType enumerates primitive physical types.
type PType int

const (
	PTypeU8 PType = iota
	PTypeU16
	PTypeU32
	PTypeU64
	PTypeI8
	PTypeI16
	PTypeI32
	PTypeI64
	PTypeF16
	PTypeF32
	PTypeF64
)

// PTypeSize returns the byte size of a primitive type.
func PTypeSize(p PType) int {
	switch p {
	case PTypeU8, PTypeI8:
		return 1
	case PTypeU16, PTypeI16, PTypeF16:
		return 2
	case PTypeU32, PTypeI32, PTypeF32:
		return 4
	case PTypeU64, PTypeI64, PTypeF64:
		return 8
	default:
		return 0
	}
}

func (d *DType) String() string {
	switch d.Kind {
	case DTypeNull:
		return "null"
	case DTypeBool:
		return maybeNullable("bool", d.Nullable)
	case DTypePrimitive:
		return maybeNullable(d.PType.String(), d.Nullable)
	case DTypeDecimal:
		return maybeNullable("decimal", d.Nullable)
	case DTypeUtf8:
		return maybeNullable("utf8", d.Nullable)
	case DTypeBinary:
		return maybeNullable("binary", d.Nullable)
	case DTypeStruct:
		return maybeNullable("struct", d.Nullable)
	case DTypeList:
		return maybeNullable("list", d.Nullable)
	case DTypeFixedSizeList:
		return maybeNullable("fixed_size_list", d.Nullable)
	case DTypeExtension:
		return maybeNullable("ext("+d.ExtensionID+")", d.Nullable)
	default:
		return "unknown"
	}
}

func (p PType) String() string {
	names := []string{"u8", "u16", "u32", "u64", "i8", "i16", "i32", "i64", "f16", "f32", "f64"}
	if int(p) < len(names) {
		return names[p]
	}
	return "unknown"
}

func maybeNullable(name string, nullable bool) string {
	if nullable {
		return name + "?"
	}
	return name
}

// String returns the name of the DType kind.
func (k DTypeKind) String() string {
	switch k {
	case DTypeNull:
		return "null"
	case DTypeBool:
		return "bool"
	case DTypePrimitive:
		return "primitive"
	case DTypeDecimal:
		return "decimal"
	case DTypeUtf8:
		return "utf8"
	case DTypeBinary:
		return "binary"
	case DTypeStruct:
		return "struct"
	case DTypeList:
		return "list"
	case DTypeFixedSizeList:
		return "fixed_size_list"
	case DTypeExtension:
		return "extension"
	default:
		return "unknown"
	}
}
