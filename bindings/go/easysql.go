// Package easysql provides a Go binding for the easysql native library.
package easysql

/*
#cgo CFLAGS: -I${SRCDIR}/../../include
#cgo darwin LDFLAGS: -L${SRCDIR}/../../lib -leasysql -Wl,-rpath,${SRCDIR}/../../lib
#cgo linux LDFLAGS: -L${SRCDIR}/../../lib -leasysql -Wl,-rpath,${SRCDIR}/../../lib
#cgo windows LDFLAGS: -L${SRCDIR}/../../lib -leasysql
#include "easysql.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"errors"
	"unsafe"
)

const ABIVersion = 1

func init() {
	if actual := uint32(C.easysql_abi_version()); actual != ABIVersion {
		panic("easysql: incompatible native ABI")
	}
}

// Version returns the native easysql library version.
func Version() string {
	value := C.easysql_version()
	if value == nil {
		return ""
	}
	defer C.easysql_free_string(value)
	return C.GoString(value)
}

// Execute sends a JSON request to the native library and returns its JSON response.
func Execute(request []byte) ([]byte, error) {
	var input unsafe.Pointer
	if len(request) > 0 {
		input = C.CBytes(request)
		defer C.free(input)
	}
	value := C.easysql_execute(input, C.size_t(len(request)))
	if value == nil {
		return nil, errors.New("easysql: native library returned a null response")
	}
	defer C.easysql_free_string(value)
	return []byte(C.GoString(value)), nil
}

// ExecuteJSON marshals request and unmarshals the native JSON response into response.
func ExecuteJSON(request any, response any) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	result, err := Execute(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(result, response)
}
