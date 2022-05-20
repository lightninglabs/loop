package loopdb

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/coreos/bbolt"
)

// DumpDB dumps go code describing the contents of the database to stdout. This
// function is only intended for use during development. Therefore also the
// linter unused warnings are suppressed.
//
// Example output:
//
// map[string]interface{}{
//      Hex("1234"): map[string]interface{}{
//              "human-readable": Hex("102030"),
//              Hex("1111"): Hex("5783492373"),
//      },
// } .
func DumpDB(tx *bbolt.Tx) error { // nolint: unused
	return tx.ForEach(func(k []byte, bucket *bbolt.Bucket) error {
		key := toString(k)
		fmt.Printf("%v: ", key)

		err := dumpBucket(bucket)
		if err != nil {
			return err
		}
		fmt.Printf(",\n")

		return nil
	})
}

func dumpBucket(bucket *bbolt.Bucket) error { // nolint: unused
	fmt.Printf("map[string]interface{} {\n")
	err := bucket.ForEach(func(k, v []byte) error {
		key := toString(k)
		fmt.Printf("%v: ", key)

		subBucket := bucket.Bucket(k)
		if subBucket != nil {
			err := dumpBucket(subBucket)
			if err != nil {
				return err
			}
		} else {
			fmt.Print(toHex(v))
		}
		fmt.Printf(",\n")

		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("}")

	return nil
}

// RestoreDB primes the database with the given data set.
func RestoreDB(tx *bbolt.Tx, data map[string]interface{}) error {
	for k, v := range data {
		key := []byte(k)

		value := v.(map[string]interface{})

		subBucket, err := tx.CreateBucket(key)
		if err != nil {
			return fmt.Errorf("create bucket %v: %v",
				string(key), err)
		}

		if err := restoreDB(subBucket, value); err != nil {
			return err
		}
	}

	return nil
}

func restoreDB(bucket *bbolt.Bucket, data map[string]interface{}) error {
	for k, v := range data {
		key := []byte(k)

		// Store nil values.
		if v == nil {
			err := bucket.Put(key, nil)
			if err != nil {
				return err
			}
			continue
		}
		switch value := v.(type) {
		// Key contains value.
		case string:
			err := bucket.Put(key, []byte(value))
			if err != nil {
				return err
			}

		// Key contains a sub-bucket.
		case map[string]interface{}:
			subBucket, err := bucket.CreateBucket(key)
			if err != nil {
				return err
			}

			if err := restoreDB(subBucket, value); err != nil {
				return err
			}

		default:
			return fmt.Errorf("invalid type %T", value)
		}
	}

	return nil
}

func toHex(v []byte) string { // nolint: unused
	if len(v) == 0 {
		return "nil"
	}

	return "Hex(\"" + hex.EncodeToString(v) + "\")"
}

func toString(v []byte) string { // nolint: unused
	readableChars := "abcdefghijklmnopqrstuvwxyz0123456789-"

	for _, c := range v {
		if !strings.Contains(readableChars, string(c)) {
			return toHex(v)
		}
	}

	return "\"" + string(v) + "\""
}

// Hex is a test helper function to convert readable hex arrays to raw byte
// strings.
func Hex(value string) string {
	b, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return string(b)
}
