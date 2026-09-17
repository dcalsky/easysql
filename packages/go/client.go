package easysql

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/dcalsky/easysql/packages/go/internal/ffi"
)

type nativeRuntime interface {
	Version() (string, error)
	Execute([]byte) ([]byte, error)
	Close() error
}

type Client struct {
	mu      sync.RWMutex
	runtime nativeRuntime
	closed  bool
}

func Open(path string) (*Client, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("easysql: library path is required")
	}
	library, err := ffi.Open(path)
	if err != nil {
		return nil, fmt.Errorf("easysql: open native library %q: %w", path, err)
	}
	if actual := library.ABIVersion(); actual != ABIVersion {
		_ = library.Close()
		return nil, fmt.Errorf("easysql: native ABI %d is incompatible with SDK ABI %d", actual, ABIVersion)
	}
	return &Client{runtime: library}, nil
}

func OpenDefault() (*Client, error) {
	candidates, err := defaultLibraryCandidates()
	if err != nil {
		return nil, err
	}
	var failures []error
	for _, candidate := range candidates {
		client, err := Open(candidate)
		if err == nil {
			return client, nil
		}
		failures = append(failures, err)
	}
	return nil, fmt.Errorf(
		"easysql: could not open native library; set %s or install the platform library: %w",
		LibraryPathEnv, errors.Join(failures...),
	)
}

func (client *Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return nil
	}
	client.closed = true
	if client.runtime == nil {
		return nil
	}
	err := client.runtime.Close()
	client.runtime = nil
	return err
}

func (client *Client) RuntimeVersion() (string, error) {
	runtime, release, err := client.use()
	if err != nil {
		return "", err
	}
	defer release()
	return runtime.Version()
}

// Execute sends a raw ABI request and returns the raw JSON response. Native
// operation errors remain encoded in the response envelope.
func (client *Client) Execute(request []byte) ([]byte, error) {
	runtime, release, err := client.use()
	if err != nil {
		return nil, err
	}
	defer release()
	return runtime.Execute(request)
}

func (client *Client) use() (nativeRuntime, func(), error) {
	client.mu.RLock()
	if client.closed || client.runtime == nil {
		client.mu.RUnlock()
		return nil, nil, ErrClosed
	}
	return client.runtime, client.mu.RUnlock, nil
}
