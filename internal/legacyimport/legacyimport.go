// Package legacyimport is the shared shape of a store's one-time legacy import,
// as the migration registry (internal/migrate) drives it.
//
// These imports used to run inside each store's constructor at daemon boot,
// guarded by a sentinel file and preceded by a wipe of the destination whenever
// the sentinel was missing. They are now explicit, offline, and additive: an
// import only ever inserts records the destination does not have yet, so it can
// be re-run after a crash at any point and can never destroy live data.
package legacyimport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/srjn45/scriva/engine"
)

// Importer is one store's legacy import. Every func takes the warden data dir.
type Importer struct {
	// Present reports whether the legacy source exists. It only stats paths.
	Present func(dataDir string) (bool, error)
	// Import copies every legacy record the destination lacks. Idempotent.
	Import func(dataDir string) error
	// Verify fails when a legacy record is still missing from the destination.
	Verify func(dataDir string) error
}

// Exists reports whether path exists, distinguishing a genuine stat error from
// a plain not-exist.
func Exists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

// HasJSON reports whether dir directly contains at least one *.json file.
func HasJSON(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			return true, nil
		}
	}
	return false, nil
}

// MergeJSONL inserts every NDJSON record whose keyField value is not already a
// key in col, and returns how many it inserted. Records are stored exactly as
// Collection.LoadJSONL stores them; unlike LoadJSONL an existing key is skipped
// rather than aborting the load, which is what makes a re-run safe.
func MergeJSONL(col *engine.Collection, r io.Reader, keyField string) (int, error) {
	inserted := 0
	err := eachRecord(r, keyField, func(key string, rec map[string]any) error {
		if _, _, err := col.InsertWithKey(key, rec); err != nil {
			if errors.Is(err, engine.ErrDuplicateKey) {
				return nil
			}
			return err
		}
		inserted++
		return nil
	})
	return inserted, err
}

// MissingJSONL returns the keys of the NDJSON records that col does not hold.
func MissingJSONL(col *engine.Collection, r io.Reader, keyField string) ([]string, error) {
	var missing []string
	err := eachRecord(r, keyField, func(key string, _ map[string]any) error {
		ok, err := col.Exists(key)
		if err != nil {
			return err
		}
		if !ok {
			missing = append(missing, key)
		}
		return nil
	})
	return missing, err
}

// VerifyNone turns a list of missing keys into a Verify result.
func VerifyNone(what string, missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	shown := missing
	if len(shown) > 5 {
		shown = shown[:5]
	}
	return fmt.Errorf("%s: %d legacy record(s) missing after import (e.g. %s)", what, len(missing), strings.Join(shown, ", "))
}

func eachRecord(r io.Reader, keyField string, fn func(key string, rec map[string]any) error) error {
	br := bufio.NewReader(r)
	for line := 1; ; line++ {
		raw, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(raw)) > 0 {
			var rec map[string]any
			if jerr := json.Unmarshal(raw, &rec); jerr != nil {
				return fmt.Errorf("legacy import: line %d: %w", line, jerr)
			}
			key, _ := rec[keyField].(string)
			if key == "" {
				return fmt.Errorf("legacy import: line %d: missing %q", line, keyField)
			}
			if ferr := fn(key, rec); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Path joins a slash-separated relative path onto the data dir.
func Path(dataDir, rel string) string {
	return filepath.Join(dataDir, filepath.FromSlash(rel))
}
