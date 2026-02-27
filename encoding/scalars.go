package encoding

import (
	"github.com/LaurieRhodes/vortex-go/internal/proto/scalar"
)

// scalarToInt64 extracts a signed integer value from a ScalarValue.
func scalarToInt64(sv *scalar.ScalarValue) int64 {
	switch v := sv.Kind.(type) {
	case *scalar.ScalarValue_Int64Value:
		return v.Int64Value
	case *scalar.ScalarValue_Uint64Value:
		return int64(v.Uint64Value)
	case *scalar.ScalarValue_F32Value:
		return int64(v.F32Value)
	case *scalar.ScalarValue_F64Value:
		return int64(v.F64Value)
	default:
		return 0
	}
}

// scalarToUint64 extracts an unsigned integer value from a ScalarValue.
func scalarToUint64(sv *scalar.ScalarValue) uint64 {
	switch v := sv.Kind.(type) {
	case *scalar.ScalarValue_Uint64Value:
		return v.Uint64Value
	case *scalar.ScalarValue_Int64Value:
		return uint64(v.Int64Value)
	case *scalar.ScalarValue_F32Value:
		return uint64(v.F32Value)
	case *scalar.ScalarValue_F64Value:
		return uint64(v.F64Value)
	default:
		return 0
	}
}

// scalarToFloat32 extracts a float32 value from a ScalarValue.
func scalarToFloat32(sv *scalar.ScalarValue) float32 {
	switch v := sv.Kind.(type) {
	case *scalar.ScalarValue_F32Value:
		return v.F32Value
	case *scalar.ScalarValue_F64Value:
		return float32(v.F64Value)
	case *scalar.ScalarValue_Int64Value:
		return float32(v.Int64Value)
	case *scalar.ScalarValue_Uint64Value:
		return float32(v.Uint64Value)
	default:
		return 0
	}
}

// scalarToFloat64 extracts a float64 value from a ScalarValue.
func scalarToFloat64(sv *scalar.ScalarValue) float64 {
	switch v := sv.Kind.(type) {
	case *scalar.ScalarValue_F64Value:
		return v.F64Value
	case *scalar.ScalarValue_F32Value:
		return float64(v.F32Value)
	case *scalar.ScalarValue_Int64Value:
		return float64(v.Int64Value)
	case *scalar.ScalarValue_Uint64Value:
		return float64(v.Uint64Value)
	default:
		return 0
	}
}
