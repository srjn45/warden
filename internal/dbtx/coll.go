package dbtx

import (
	"fmt"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/store"
)

// Codec converts a typed value to/from the stored document.
type Codec[T any] interface {
	Encode(T) (map[string]any, error)
	Decode(map[string]any) (T, error)
}

// MapCodec is the identity codec for untyped documents.
type MapCodec struct{}

func (MapCodec) Encode(m map[string]any) (map[string]any, error) { return m, nil }
func (MapCodec) Decode(m map[string]any) (map[string]any, error) { return m, nil }

// Versioned is a read value with its revision (feed Rev to ExpectedRev).
type Versioned[T any] struct {
	ID    uint64
	Rev   uint64
	Value T
}

// Coll is a typed handle to a declared, already-created collection.
type Coll[T any] struct {
	b     *Boundary
	name  string
	col   collection
	codec Codec[T]
}

// Bind declares a collection on the boundary. The collection must already
// exist (bootstrap/migration creates collections and indexes; creation is not
// transactional — audit G8); a missing one yields ClassCollectionNotFound.
func Bind[T any](b *Boundary, name string, codec Codec[T]) (*Coll[T], error) {
	col, err := b.eng.Collection(name)
	if err != nil {
		return nil, &Error{Class: Classify(err), Err: err}
	}
	return &Coll[T]{b: b, name: name, col: col, codec: codec}, nil
}

func (c *Coll[T]) Name() string { return c.name }

// Get reads outside any transaction (no snapshot, audit I1/I2).
func (c *Coll[T]) Get(id uint64) (Versioned[T], error) {
	rec, err := c.col.Get(id)
	if err != nil {
		return Versioned[T]{}, err
	}
	v, err := c.codec.Decode(rec.Data)
	if err != nil {
		return Versioned[T]{}, err
	}
	return Versioned[T]{ID: rec.ID, Rev: rec.Rev, Value: v}, nil
}

// Insert stages a create and returns its id. The id always comes from the
// collection's ReserveID, never from the caller (audit G3/A1). A failed or
// rolled-back transaction burns the id.
func (c *Coll[T]) Insert(tx *Tx, v T) (uint64, error) {
	data, err := c.codec.Encode(v)
	if err != nil {
		return 0, internal(fmt.Errorf("%w: encode: %v", ErrInvalidOp, err))
	}
	id := c.col.ReserveID()
	if err := tx.stage(c.b, engine.XTxOp{Collection: c.name, Op: store.OpInsert, ID: id, Data: data}); err != nil {
		return 0, err
	}
	return id, nil
}

// Update stages a replace of id. expectedRev 0 means unconditional.
func (c *Coll[T]) Update(tx *Tx, id uint64, v T, expectedRev uint64) error {
	if id == 0 {
		return internal(fmt.Errorf("%w: zero id", ErrInvalidOp))
	}
	data, err := c.codec.Encode(v)
	if err != nil {
		return internal(fmt.Errorf("%w: encode: %v", ErrInvalidOp, err))
	}
	return tx.stage(c.b, engine.XTxOp{Collection: c.name, Op: store.OpUpdate, ID: id, Data: data, ExpectedRev: expectedRev})
}

// Delete stages removal of id. expectedRev 0 means unconditional.
func (c *Coll[T]) Delete(tx *Tx, id uint64, expectedRev uint64) error {
	if id == 0 {
		return internal(fmt.Errorf("%w: zero id", ErrInvalidOp))
	}
	return tx.stage(c.b, engine.XTxOp{Collection: c.name, Op: store.OpDelete, ID: id, ExpectedRev: expectedRev})
}
