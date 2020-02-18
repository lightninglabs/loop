package lsat

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
)

// TestStore tests the basic functionality of the file based store.
func TestFileStore(t *testing.T) {
	t.Parallel()

	tempDirName, err := ioutil.TempDir("", "lsatstore")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDirName)

	var (
		paidPreimage = lntypes.Preimage{1, 2, 3, 4, 5}
		paidToken    = &Token{
			Preimage: paidPreimage,
			baseMac:  makeMac(),
		}
		pendingToken = &Token{
			Preimage: zeroPreimage,
			baseMac:  makeMac(),
		}
	)

	store, err := NewFileStore(tempDirName)
	if err != nil {
		t.Fatalf("could not create test store: %v", err)
	}

	// Make sure the current store is empty.
	_, err = store.CurrentToken()
	if err != ErrNoToken {
		t.Fatalf("expected store to be empty but error was: %v", err)
	}
	tokens, err := store.AllTokens()
	if err != nil {
		t.Fatalf("unexpected error listing all tokens: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("expected store to be empty but got %v", tokens)
	}

	// Store a pending token and make sure we can read it again.
	err = store.StoreToken(pendingToken)
	if err != nil {
		t.Fatalf("could not save pending token: %v", err)
	}
	if !fileExists(filepath.Join(tempDirName, storeFileNamePending)) {
		t.Fatalf("expected file %s/%s to exist but it didn't",
			tempDirName, storeFileNamePending)
	}
	token, err := store.CurrentToken()
	if err != nil {
		t.Fatalf("could not read pending token: %v", err)
	}
	if !token.baseMac.Equal(pendingToken.baseMac) {
		t.Fatalf("expected macaroon to match")
	}
	tokens, err = store.AllTokens()
	if err != nil {
		t.Fatalf("unexpected error listing all tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("unexpected number of tokens, got %d expected %d",
			len(tokens), 1)
	}
	for key := range tokens {
		if !tokens[key].baseMac.Equal(pendingToken.baseMac) {
			t.Fatalf("expected macaroon to match")
		}
	}

	// Replace the pending token with a final one and make sure the pending
	// token was replaced.
	err = store.StoreToken(paidToken)
	if err != nil {
		t.Fatalf("could not save pending token: %v", err)
	}
	if !fileExists(filepath.Join(tempDirName, storeFileName)) {
		t.Fatalf("expected file %s/%s to exist but it didn't",
			tempDirName, storeFileName)
	}
	if fileExists(filepath.Join(tempDirName, storeFileNamePending)) {
		t.Fatalf("expected file %s/%s to be removed but it wasn't",
			tempDirName, storeFileNamePending)
	}
	token, err = store.CurrentToken()
	if err != nil {
		t.Fatalf("could not read pending token: %v", err)
	}
	if !token.baseMac.Equal(paidToken.baseMac) {
		t.Fatalf("expected macaroon to match")
	}
	tokens, err = store.AllTokens()
	if err != nil {
		t.Fatalf("unexpected error listing all tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("unexpected number of tokens, got %d expected %d",
			len(tokens), 1)
	}
	for key := range tokens {
		if !tokens[key].baseMac.Equal(paidToken.baseMac) {
			t.Fatalf("expected macaroon to match")
		}
	}

	// Make sure we can't replace the existing paid token with a pending.
	err = store.StoreToken(pendingToken)
	if err != errNoReplace {
		t.Fatalf("unexpected error. got %v, expected %v", err,
			errNoReplace)
	}

	// Make sure we can also not overwrite the existing paid token with a
	// new paid one.
	err = store.StoreToken(paidToken)
	if err != errNoReplace {
		t.Fatalf("unexpected error. got %v, expected %v", err,
			errNoReplace)
	}
}
