package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type setupFileSnapshot struct {
	path    string
	existed bool
	mode    fs.FileMode
	data    []byte
}

type setupTransaction struct {
	config         setupFileSnapshot
	marker         setupFileSnapshot
	dataDir        string
	dataDirCreated bool
	dataDirMode    fs.FileMode
}

func beginSetupTransaction(configPath, dataDir string) (*setupTransaction, error) {
	configuration, err := snapshotSetupFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("snapshot Layer Cache configuration: %w", err)
	}
	marker, err := snapshotSetupFile(filepath.Join(dataDir, ownershipMarkerName))
	if err != nil {
		return nil, fmt.Errorf("snapshot Local Cache ownership: %w", err)
	}
	dataDirInfo, statErr := os.Stat(dataDir)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect Local Cache directory before setup: %w", statErr)
	}
	dataDirMode := fs.FileMode(0)
	if dataDirInfo != nil {
		dataDirMode = dataDirInfo.Mode().Perm()
	}
	return &setupTransaction{
		config: configuration, marker: marker, dataDir: dataDir,
		dataDirCreated: errors.Is(statErr, fs.ErrNotExist),
		dataDirMode:    dataDirMode,
	}, nil
}

func snapshotSetupFile(path string) (setupFileSnapshot, error) {
	snapshot := setupFileSnapshot{path: path}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return setupFileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return setupFileSnapshot{}, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return setupFileSnapshot{}, err
	}
	snapshot.existed = true
	snapshot.mode = info.Mode().Perm()
	snapshot.data = data
	return snapshot, nil
}

func (transaction *setupTransaction) Rollback() error {
	if transaction == nil {
		return nil
	}
	var rollbackErrors []error
	if err := restoreSetupFile(transaction.marker); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("restore Local Cache ownership: %w", err))
	}
	if err := restoreSetupFile(transaction.config); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("restore Layer Cache configuration: %w", err))
	}
	if transaction.dataDirCreated {
		if err := os.Remove(transaction.dataDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove setup-created Local Cache directory: %w", err))
		}
	} else if transaction.dataDirMode != 0 {
		if err := os.Chmod(transaction.dataDir, transaction.dataDirMode); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore Local Cache directory permissions: %w", err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func restoreSetupFile(snapshot setupFileSnapshot) error {
	if !snapshot.existed {
		if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(snapshot.path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(snapshot.path), ".layercache-rollback-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(snapshot.mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(snapshot.data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, snapshot.path)
}
