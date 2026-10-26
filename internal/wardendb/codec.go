package wardendb

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// LosslessDocument encodes a complete owning payload without rounding JSON
// numbers. Scriva v1.6 verifies checksums through float64; refuse values that
// its verifier cannot represent, rather than producing an unreadable record.
func LosslessDocument(value any) (map[string]any, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&document); err != nil {
		return nil, err
	}
	var engineView map[string]any
	if err := json.Unmarshal(b, &engineView); err != nil {
		return nil, err
	}
	exact, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	engineBytes, err := json.Marshal(engineView)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(exact, engineBytes) {
		return nil, fmt.Errorf("wardendb: numeric payload is not losslessly representable by the storage engine")
	}
	return document, nil
}

// StrictRecord decodes full owning types and rejects fields their codec cannot
// preserve. Engine metadata is not a domain field.
func StrictRecord(document map[string]any, dst any, forbidden ...string) error {
	copy := make(map[string]any, len(document))
	for field, value := range document {
		if field != "_key" {
			copy[field] = value
		}
	}
	for _, field := range forbidden {
		if _, exists := copy[field]; exists {
			return fmt.Errorf("wardendb: forbidden persisted field %q", field)
		}
	}
	b, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
