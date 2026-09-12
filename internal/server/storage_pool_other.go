//go:build !linux && !darwin

package server

import "errors"

type StoragePoolConfig struct {
	Path             string `json:"path"`
	MaxBytes         int64  `json:"maxBytes"`
	MinFreeBytes     int64  `json:"minFreeBytes"`
	HostPath         string `json:"hostPath"`
	HostMinFreeBytes int64  `json:"hostMinFreeBytes"`
}

func (p StoragePoolConfig) validate() error {
	if p.Path != "" {
		return errors.New("bounded storage pools require Linux or macOS")
	}
	return nil
}
func (p StoragePoolConfig) admits(_ int64) bool    { return p.Path == "" }
func (p StoragePoolConfig) contains(_ string) bool { return p.Path == "" }

func (p StoragePoolConfig) registryLease() (func(), error)   { return func() {}, nil }
func (p StoragePoolConfig) touchRegistry(_ string, _ string) {}

func (p StoragePoolConfig) status() map[string]any { return map[string]any{"available": false} }
