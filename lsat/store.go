package lsat

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
)

var (
	// ErrNoToken is the error returned when the store doesn't contain a
	// token yet.
	ErrNoToken = fmt.Errorf("no token in store")

	storeFileName = "lsat.token"
)

// Store is an interface that allows users to store and retrieve an LSAT token.
type Store interface {
	// HasToken returns true if the store contains a token.
	HasToken() bool

	// Token returns the token that is contained in the store or an error
	// if there is none.
	Token() (*Token, error)

	// StoreToken saves a token to the store, overwriting any old token if
	// there is one.
	StoreToken(*Token) error
}

// FileStore is an implementation of the Store interface that uses a single file
// to save the serialized token.
type FileStore struct {
	fileName string
}

// A compile-time flag to ensure that FileStore implements the Store interface.
var _ Store = (*FileStore)(nil)

// NewFileStore creates a new file based token store, creating its file in the
// provided directory. If the directory does not exist, it will be created.
func NewFileStore(storeDir string) (*FileStore, error) {
	// If the target path for the token store doesn't exist, then we'll
	// create it now before we proceed.
	if !fileExists(storeDir) {
		if err := os.MkdirAll(storeDir, 0700); err != nil {
			return nil, err
		}
	}

	return &FileStore{
		fileName: filepath.Join(storeDir, storeFileName),
	}, nil
}

// HasToken returns true if the store contains a token.
//
// NOTE: This is part of the Store interface.
func (f *FileStore) HasToken() bool {
	return fileExists(f.fileName)
}

// Token returns the token that is contained in the store or an error if there
// is none.
//
// NOTE: This is part of the Store interface.
func (f *FileStore) Token() (*Token, error) {
	if !f.HasToken() {
		return nil, ErrNoToken
	}
	bytes, err := ioutil.ReadFile(f.fileName)
	if err != nil {
		return nil, err
	}
	return deserializeToken(bytes)
}

// StoreToken saves a token to the store, overwriting any old token if there is
// one.
//
// NOTE: This is part of the Store interface.
func (f *FileStore) StoreToken(token *Token) error {
	bytes, err := serializeToken(token)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(f.fileName, bytes, 0600)
}

// fileExists returns true if the file exists, and false otherwise.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}
