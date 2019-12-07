package lsat

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrNoToken is the error returned when the store doesn't contain a
	// token yet.
	ErrNoToken = errors.New("no token in store")

	// storeFileName is the name of the file where we store the final,
	// valid, token to.
	storeFileName = "lsat.token"

	// storeFileNamePending is the name of the file where we store a pending
	// token until it was successfully paid for.
	storeFileNamePending = "lsat.token.pending"

	// errNoReplace is the error that is returned if a new token is
	// being written to a store that already contains a paid token.
	errNoReplace = errors.New("won't replace existing paid token with " +
		"new token. " + manualRetryHint)
)

// Store is an interface that allows users to store and retrieve an LSAT token.
type Store interface {
	// CurrentToken returns the token that is currently contained in the
	// store or an error if there is none.
	CurrentToken() (*Token, error)

	// AllTokens returns all tokens that the store has knowledge of, even
	// if they might be expired. The tokens are mapped by their identifying
	// attribute like file name or storage key.
	AllTokens() (map[string]*Token, error)

	// StoreToken saves a token to the store. Old tokens should be kept for
	// accounting purposes but marked as invalid somehow.
	StoreToken(*Token) error
}

// FileStore is an implementation of the Store interface that files to save the
// serialized tokens. There is always just one current token that is either
// pending or fully paid.
type FileStore struct {
	fileName        string
	fileNamePending string
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
		fileName:        filepath.Join(storeDir, storeFileName),
		fileNamePending: filepath.Join(storeDir, storeFileNamePending),
	}, nil
}

// CurrentToken returns the token that is currently contained in the store or an
// error if there is none.
//
// NOTE: This is part of the Store interface.
func (f *FileStore) CurrentToken() (*Token, error) {
	// As this is only a wrapper for external users to make sure the store
	// is locked, the actual implementation is in the non-exported method.
	return f.currentToken()
}

// currentToken returns the current token without locking the store.
func (f *FileStore) currentToken() (*Token, error) {
	switch {
	case fileExists(f.fileName):
		return readTokenFile(f.fileName)

	case fileExists(f.fileNamePending):
		return readTokenFile(f.fileNamePending)

	default:
		return nil, ErrNoToken
	}
}

// AllTokens returns all tokens that the store has knowledge of, even if they
// might be expired. The tokens are mapped by their identifying attribute like
// file name or storage key.
//
// NOTE: This is part of the Store interface.
func (f *FileStore) AllTokens() (map[string]*Token, error) {
	tokens := make(map[string]*Token)

	// All tokens start with the same name so we can get them by the prefix.
	// As the tokens don't expire yet, there currently can't be more than
	// just one token, either pending or paid.
	// TODO(guggero): Update comment once tokens expire and we keep backups.
	tokenDir := filepath.Dir(f.fileName)
	files, err := ioutil.ReadDir(tokenDir)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		name := file.Name()
		if !strings.HasPrefix(name, storeFileName) {
			continue
		}
		fileName := filepath.Join(tokenDir, name)
		token, err := readTokenFile(fileName)
		if err != nil {
			return nil, err
		}
		tokens[fileName] = token
	}

	return tokens, nil
}

// StoreToken saves a token to the store, overwriting any old token if there is
// one.
//
// NOTE: This is part of the Store interface.
func (f *FileStore) StoreToken(newToken *Token) error {
	// Serialize the token first, before we rename anything.
	bytes, err := serializeToken(newToken)
	if err != nil {
		return err
	}

	// We'll need to know if there is any other token already in place,
	// either pending or not, that we need to delete or overwrite.
	currentToken, err := f.currentToken()

	switch {
	// No token in the store yet, just write it to the corresponding file.
	case err == ErrNoToken:
		// What's the target file name we are going to write?
		newFileName := f.fileName
		if newToken.isPending() {
			newFileName = f.fileNamePending
		}
		return ioutil.WriteFile(newFileName, bytes, 0600)

	// Fail on any other error.
	case err != nil:
		return err

	// Replace a pending token with a paid one.
	case currentToken.isPending() && !newToken.isPending():
		// Make sure we replace the the same token, just with a
		// different state.
		if currentToken.PaymentHash != newToken.PaymentHash {
			return fmt.Errorf("new paid token doesn't match " +
				"existing pending token")
		}

		// Write the new token first, so we still have the pending
		// around if something goes wrong.
		err := ioutil.WriteFile(f.fileName, bytes, 0600)
		if err != nil {
			return err
		}

		// We were able to write the new token so removing the old one
		// can be just best effort. By default, the valid one will be
		// read by the store if both exist.
		_ = os.Remove(f.fileNamePending)
		return nil

	// Catch all, we get here if an existing token is attempted to be
	// replaced with another token outside of the pending->paid flow. The
	// user should manually remove the token in that case.
	// TODO(guggero): Once tokens expire, this logic has to be adapted
	//  accordingly.
	default:
		return errNoReplace
	}
}

// readTokenFile reads a single token from a file and returns it deserialized.
func readTokenFile(tokenFile string) (*Token, error) {
	bytes, err := ioutil.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}
	return deserializeToken(bytes)
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
