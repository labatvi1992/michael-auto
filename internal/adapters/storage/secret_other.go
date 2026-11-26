//go:build !linux

package storage

import (
	"errors"
	"sync"
)

// FreedesktopSecretStore provides an in-memory secret store stub on non-Linux targets.
type FreedesktopSecretStore struct {
	mu   sync.Mutex
	data map[string]string
}

func NewSecretStore() *FreedesktopSecretStore {
	return &FreedesktopSecretStore{data: make(map[string]string)}
}

func (s *FreedesktopSecretStore) Backend() string {
	return "memory-store"
}

func (s *FreedesktopSecretStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	if !ok {
		return "", errors.New("secret not found")
	}
	return v, nil
}

func (s *FreedesktopSecretStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

func (s *FreedesktopSecretStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}
