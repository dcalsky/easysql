//go:build linux

package main

/*
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>
#cgo LDFLAGS: -ldl

static char *easysql_module_path(void) {
	Dl_info info;
	if (dladdr((void *)&easysql_module_path, &info) == 0 || info.dli_fname == NULL) {
		return NULL;
	}
	return strdup(info.dli_fname);
}
*/
import "C"

import (
	"errors"
	"path/filepath"
	"unsafe"
)

func moduleDirectory() (string, error) {
	path := C.easysql_module_path()
	if path == nil {
		return "", errors.New("easysql: cannot locate native library")
	}
	defer C.free(unsafe.Pointer(path))
	abs, err := filepath.Abs(C.GoString(path))
	if err != nil {
		return "", err
	}
	return filepath.Dir(abs), nil
}
