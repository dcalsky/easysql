package main

/*
#include <stdint.h>
#include <stdlib.h>
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

//export easysql_execute
func easysql_execute(data unsafe.Pointer, length C.size_t) (output *C.char) {
	defer func() {
		if recover() != nil {
			output = C.CString(string(nativeffi.InternalFailure("easysql: native adapter panic")))
		}
	}()

	if uint64(length) > maxNativeRequestBytes {
		return C.CString(string(nativeffi.InvalidArgumentFailure(fmt.Sprintf(
			"easysql: native request exceeds %d bytes", maxNativeRequestBytes))))
	}
	if err := initializeRuntime(); err != nil {
		return C.CString(string(nativeffi.InternalFailure(err.Error())))
	}

	var input []byte
	if data != nil && length > 0 {
		input = C.GoBytes(data, C.int(length))
	}
	return C.CString(string(nativeffi.Execute(input)))
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
