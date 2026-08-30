//go:build windows

package main

/*
#include <windows.h>
#include <stdlib.h>

static char *easysql_module_path(void) {
	HMODULE module = NULL;
	if (!GetModuleHandleExW(
		GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
		(LPCWSTR)&easysql_module_path,
		&module)) {
		return NULL;
	}

	wchar_t wide_path[32768];
	DWORD length = GetModuleFileNameW(module, wide_path, 32768);
	if (length == 0 || length >= 32768) {
		return NULL;
	}
	int size = WideCharToMultiByte(CP_UTF8, 0, wide_path, (int)length, NULL, 0, NULL, NULL);
	if (size <= 0) {
		return NULL;
	}
	char *path = (char *)malloc((size_t)size + 1);
	if (path == NULL) {
		return NULL;
	}
	WideCharToMultiByte(CP_UTF8, 0, wide_path, (int)length, path, size, NULL, NULL);
	path[size] = '\0';
	return path;
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
