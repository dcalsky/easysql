package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/dcalsky/easysql"
	"github.com/dcalsky/easysql/internal/nativeffi"
)

const maxNativeRequestBytes = 16 << 20

var (
	runtimeOnce sync.Once
	runtimeErr  error
)

//export easysql_abi_version
func easysql_abi_version() C.uint32_t {
	return C.uint32_t(nativeffi.ABIVersion)
}

//export easysql_version
func easysql_version() *C.char {
	return C.CString(nativeffi.LibraryVersion)
}

//export easysql_version_into
func easysql_version_into(output unsafe.Pointer, capacity C.size_t) C.size_t {
	return copyToNativeBuffer([]byte(nativeffi.LibraryVersion), output, capacity)
}

//export easysql_execute
func easysql_execute(data unsafe.Pointer, length C.size_t) *C.char {
	return C.CString(string(executeRequest(data, length)))
}

//export easysql_execute_into
func easysql_execute_into(data unsafe.Pointer, length C.size_t, output unsafe.Pointer, capacity C.size_t) C.size_t {
	return copyToNativeBuffer(executeRequest(data, length), output, capacity)
}

func executeRequest(data unsafe.Pointer, length C.size_t) (output []byte) {
	defer func() {
		if recover() != nil {
			output = nativeffi.InternalFailure("easysql: native adapter panic")
		}
	}()

	if uint64(length) > maxNativeRequestBytes {
		return nativeffi.InvalidArgumentFailure(fmt.Sprintf(
			"easysql: native request exceeds %d bytes", maxNativeRequestBytes))
	}
	if err := initializeRuntime(); err != nil {
		return nativeffi.InternalFailure(err.Error())
	}

	var input []byte
	if data != nil && length > 0 {
		input = C.GoBytes(data, C.int(length))
	}
	return nativeffi.Execute(input)
}

func copyToNativeBuffer(value []byte, output unsafe.Pointer, capacity C.size_t) C.size_t {
	required := C.size_t(len(value) + 1)
	if output == nil || capacity == 0 {
		return required
	}
	writable := len(value)
	if capacity <= C.size_t(len(value)) {
		writable = int(capacity) - 1
	}
	if writable > 0 {
		C.memcpy(output, unsafe.Pointer(&value[0]), C.size_t(writable))
	}
	*(*byte)(unsafe.Add(output, writable)) = 0
	return required
}

//export easysql_free_string
func easysql_free_string(value *C.char) {
	C.free(unsafe.Pointer(value))
}

func initializeRuntime() error {
	runtimeOnce.Do(func() {
		runtimeErr = easysql.Init()
	})
	return runtimeErr
}

func main() {}
