package unsafeconv

import "unsafe"

func StringToBytes(s string) []byte {
	if s == "" {
		return nil
	}

	//nolint:gosec // Safe for read-only transmission
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

func BytesToString(b []byte) string {
	//nolint:gosec // Safe for read-only transmission
	return unsafe.String(unsafe.SliceData(b), len(b))
}
