//go:build windows

package ffi

import "syscall"

type dynamicLibrary struct {
	dll *syscall.DLL
}

func openDynamicLibrary(path string) (*dynamicLibrary, error) {
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return nil, err
	}
	return &dynamicLibrary{dll: dll}, nil
}

func (library *dynamicLibrary) lookup(name string) (uintptr, error) {
	procedure, err := library.dll.FindProc(name)
	if err != nil {
		return 0, err
	}
	return procedure.Addr(), nil
}

func (library *dynamicLibrary) close() error {
	// A Go c-shared DLL owns process runtime state and must stay loaded until
	// process exit. Client.Close prevents further SDK calls but deliberately
	// keeps this Windows loader reference pinned.
	return nil
}
