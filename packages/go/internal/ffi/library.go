package ffi

import (
	"fmt"
	"math"

	"github.com/ebitengine/purego"
)

const maxResponseBytes = 256 << 20

type Library struct {
	dynamic *dynamicLibrary
	path    string

	abiVersion  func() uint32
	versionInto func([]byte, uintptr) uintptr
	executeInto func([]byte, uintptr, []byte, uintptr) uintptr
}

func Open(path string) (*Library, error) {
	dynamic, err := openDynamicLibrary(path)
	if err != nil {
		return nil, err
	}
	library := &Library{dynamic: dynamic, path: path}
	if err := library.register("easysql_abi_version", &library.abiVersion); err != nil {
		_ = dynamic.close()
		return nil, err
	}
	if err := library.register("easysql_version_into", &library.versionInto); err != nil {
		_ = dynamic.close()
		return nil, err
	}
	if err := library.register("easysql_execute_into", &library.executeInto); err != nil {
		_ = dynamic.close()
		return nil, err
	}
	return library, nil
}

func (library *Library) register(name string, target any) (err error) {
	address, err := library.dynamic.lookup(name)
	if err != nil {
		return fmt.Errorf("missing symbol %s: %w", name, err)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("register symbol %s: %v", name, recovered)
		}
	}()
	purego.RegisterFunc(target, address)
	return nil
}

func (library *Library) Path() string { return library.path }

func (library *Library) ABIVersion() uint32 { return library.abiVersion() }

func (library *Library) Version() (string, error) {
	data, err := readInto(func(output []byte, capacity uintptr) uintptr {
		return library.versionInto(output, capacity)
	})
	if err != nil {
		return "", fmt.Errorf("read native version: %w", err)
	}
	return string(data), nil
}

func (library *Library) Execute(request []byte) ([]byte, error) {
	data, err := readInto(func(output []byte, capacity uintptr) uintptr {
		return library.executeInto(request, uintptr(len(request)), output, capacity)
	})
	if err != nil {
		return nil, fmt.Errorf("execute native request: %w", err)
	}
	return data, nil
}

func readInto(call func([]byte, uintptr) uintptr) ([]byte, error) {
	required := call(nil, 0)
	for attempt := 0; attempt < 3; attempt++ {
		if required == 0 {
			return nil, fmt.Errorf("native function returned zero capacity")
		}
		if required > maxResponseBytes || required > uintptr(math.MaxInt) {
			return nil, fmt.Errorf("native response capacity %d exceeds limit", required)
		}
		buffer := make([]byte, int(required))
		actual := call(buffer, uintptr(len(buffer)))
		if actual > uintptr(len(buffer)) {
			required = actual
			continue
		}
		if actual == 0 || buffer[actual-1] != 0 {
			return nil, fmt.Errorf("native response is not NUL terminated")
		}
		return buffer[:actual-1], nil
	}
	return nil, fmt.Errorf("native response size changed repeatedly")
}

func (library *Library) Close() error {
	if library == nil || library.dynamic == nil {
		return nil
	}
	err := library.dynamic.close()
	library.dynamic = nil
	return err
}
