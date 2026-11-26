//go:build linux

package storage

import (
	"errors"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	secretServiceDest = "org.freedesktop.secrets"
	secretServicePath = dbus.ObjectPath("/org/freedesktop/secrets")
	defaultCollection = dbus.ObjectPath("/org/freedesktop/secrets/aliases/default")
	appSecretAttr     = "michael-auto"
)

type secretStruct struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// FreedesktopSecretStore stores OS session tokens (such as Wayland RemoteDesktop restore_token)
// in GNOME Keyring / KWallet via org.freedesktop.secrets over pure-Go D-Bus, with in-memory fallback.
type FreedesktopSecretStore struct {
	mu       sync.Mutex
	fallback map[string]string
	backend  string
	getFn    func(key string) (string, error)
	setFn    func(key, value string) error
	delFn    func(key string) error
}

// NewSecretStore creates a Linux Secret Service store with safe fallback if no keyring daemon is running.
func NewSecretStore() *FreedesktopSecretStore {
	s := &FreedesktopSecretStore{
		fallback: make(map[string]string),
		backend:  "freedesktop-secret-service",
	}
	s.getFn = s.dbusGet
	s.setFn = s.dbusSet
	s.delFn = s.dbusDelete
	return s
}

// Backend reports the active secret storage mechanism.
func (s *FreedesktopSecretStore) Backend() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backend
}

// Get retrieves a secret value by key.
func (s *FreedesktopSecretStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	val, err := s.getFn(key)
	if err == nil {
		return val, nil
	}
	if fb, ok := s.fallback[key]; ok {
		return fb, nil
	}
	return "", err
}

// Set stores a secret value in Secret Service (or memory fallback if session keyring is unavailable).
func (s *FreedesktopSecretStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.fallback[key] = value
	if err := s.setFn(key, value); err != nil {
		s.backend = "memory-fallback"
	} else {
		s.backend = "freedesktop-secret-service"
	}
	return nil
}

// Delete removes a secret value by key.
func (s *FreedesktopSecretStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fallback, key)
	_ = s.delFn(key)
	return nil
}

func (s *FreedesktopSecretStore) dbusGet(key string) (string, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	svc := conn.Object(secretServiceDest, secretServicePath)
	var algoOutput dbus.Variant
	var sessionPath dbus.ObjectPath
	if err := svc.Call("org.freedesktop.Secret.Service.OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&algoOutput, &sessionPath); err != nil {
		return "", err
	}
	defer conn.Object(secretServiceDest, sessionPath).Call("org.freedesktop.Secret.Session.Close", 0)

	attrs := map[string]string{
		"application": appSecretAttr,
		"key":         key,
	}
	var unlocked, locked []dbus.ObjectPath
	if err := svc.Call("org.freedesktop.Secret.Service.SearchItems", 0, attrs).Store(&unlocked, &locked); err != nil {
		return "", err
	}
	if len(unlocked) == 0 {
		if len(locked) > 0 {
			return "", errors.New("gnome keyring đang bị khóa (cần mở khóa phiên)")
		}
		return "", errors.New("secret not found")
	}

	var secrets map[dbus.ObjectPath]secretStruct
	if err := svc.Call("org.freedesktop.Secret.Service.GetSecrets", 0, unlocked[:1], sessionPath).Store(&secrets); err != nil {
		return "", err
	}
	sec, ok := secrets[unlocked[0]]
	if !ok {
		return "", errors.New("secret payload missing")
	}
	return string(sec.Value), nil
}

func (s *FreedesktopSecretStore) dbusSet(key, value string) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	defer conn.Close()

	svc := conn.Object(secretServiceDest, secretServicePath)
	var algoOutput dbus.Variant
	var sessionPath dbus.ObjectPath
	if err := svc.Call("org.freedesktop.Secret.Service.OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&algoOutput, &sessionPath); err != nil {
		return err
	}
	defer conn.Object(secretServiceDest, sessionPath).Call("org.freedesktop.Secret.Session.Close", 0)

	props := map[string]dbus.Variant{
		"org.freedesktop.Secret.Item.Label": dbus.MakeVariant("Michael Auto: " + key),
		"org.freedesktop.Secret.Item.Attributes": dbus.MakeVariant(map[string]string{
			"application": appSecretAttr,
			"key":         key,
		}),
	}
	sec := secretStruct{
		Session:     sessionPath,
		Parameters:  []byte{},
		Value:       []byte(value),
		ContentType: "text/plain",
	}

	coll := conn.Object(secretServiceDest, defaultCollection)
	var itemPath, promptPath dbus.ObjectPath
	return coll.Call("org.freedesktop.Secret.Collection.CreateItem", 0, props, sec, true).Store(&itemPath, &promptPath)
}

func (s *FreedesktopSecretStore) dbusDelete(key string) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	defer conn.Close()

	svc := conn.Object(secretServiceDest, secretServicePath)
	attrs := map[string]string{
		"application": appSecretAttr,
		"key":         key,
	}
	var unlocked, locked []dbus.ObjectPath
	if err := svc.Call("org.freedesktop.Secret.Service.SearchItems", 0, attrs).Store(&unlocked, &locked); err != nil {
		return err
	}
	for _, p := range unlocked {
		var prompt dbus.ObjectPath
		_ = conn.Object(secretServiceDest, p).Call("org.freedesktop.Secret.Item.Delete", 0).Store(&prompt)
	}
	return nil
}
