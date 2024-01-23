package utils

import (
	"reflect"
	"unsafe"
)

// UnsafeStringToBytes converts string to []byte without memory copy.
// The return []byte is readonly since it ref to the string's content,
// and golang strings are immutable.
func UnsafeStringToBytes(str string) []byte {
	var buf []byte
	*(*string)(unsafe.Pointer(&buf)) = str
	(*reflect.SliceHeader)(unsafe.Pointer(&buf)).Cap = len(str)
	return buf
}

// UnsafeBytesToString converts []bytes to string without memory copy.
// The return string is not immutable since it ref to the []byte's content,
// and []byte is writable.
func UnsafeBytesToString(raw []byte) string {
	return *(*string)(unsafe.Pointer(&raw))
}
