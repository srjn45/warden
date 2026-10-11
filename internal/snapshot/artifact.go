package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Artifact kinds a snapshot record may reference. Large blobs (transcripts,
// worktree archives, logs) are never stored in the database; the record carries
// an ArtifactRef instead.
const (
	ArtifactTranscript = "transcript"
	ArtifactWorktree   = "worktree_archive"
	ArtifactLog        = "log"
)

// ArtifactRef is the canonical reference from a metadata record to an external
// artifact on disk (warden-db schema-2 contract: intentional external artifact,
// pointer + size + checksum, never inlined).
type ArtifactRef struct {
	Kind string `json:"kind"`
	// Path is relative to the warden data dir, slash-separated, never escaping it.
	Path string `json:"path"`
	// Size is the byte length recorded when the artifact was written.
	Size int64 `json:"size"`
	// SHA256 is the hex content hash, recorded when cheaply available (empty otherwise).
	SHA256 string `json:"sha256,omitempty"`
}

// Artifact reconcile statuses.
const (
	ArtifactOK           = "ok"
	ArtifactMissing      = "missing"
	ArtifactSizeMismatch = "size_mismatch"
	ArtifactHashMismatch = "hash_mismatch"
	ArtifactUnsafePath   = "unsafe_path"
	ArtifactError        = "error"
)

// ArtifactFinding reports one artifact's reconcile outcome. Reconcile only
// reports; it never deletes, rewrites, or "repairs" the artifact or record.
type ArtifactFinding struct {
	Ref        ArtifactRef `json:"ref"`
	Status     string      `json:"status"`
	ActualSize int64       `json:"actual_size,omitempty"`
	Detail     string      `json:"detail,omitempty"`
}

// OK reports whether the artifact matched its reference.
func (f ArtifactFinding) OK() bool { return f.Status == ArtifactOK }

// transcriptRef builds the reference for a transcript blob this store just wrote.
func (s *Store) transcriptRef(id, transcript string) ArtifactRef {
	sum := sha256.Sum256([]byte(transcript))
	rel, err := filepath.Rel(s.dataDir, s.transcriptPath(id))
	if err != nil {
		rel = id + ".transcript"
	}
	return ArtifactRef{
		Kind:   ArtifactTranscript,
		Path:   filepath.ToSlash(rel),
		Size:   int64(len(transcript)),
		SHA256: hex.EncodeToString(sum[:]),
	}
}

// withArtifact replaces any existing ref of the same kind+path, else appends.
func withArtifact(refs []ArtifactRef, ref ArtifactRef) []ArtifactRef {
	for i := range refs {
		if refs[i].Kind == ref.Kind && refs[i].Path == ref.Path {
			refs[i] = ref
			return refs
		}
	}
	return append(refs, ref)
}

// ReconcileArtifact checks ref against the file under dataDir. A recorded hash is
// verified only when non-empty. Nothing on disk is modified.
func ReconcileArtifact(dataDir string, ref ArtifactRef) ArtifactFinding {
	f := ArtifactFinding{Ref: ref}
	clean := filepath.Clean(filepath.FromSlash(ref.Path))
	if ref.Path == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		f.Status, f.Detail = ArtifactUnsafePath, "path is empty, absolute, or escapes the data dir"
		return f
	}
	full := filepath.Join(dataDir, clean)
	fi, err := os.Lstat(full)
	switch {
	case errors.Is(err, os.ErrNotExist):
		f.Status = ArtifactMissing
		return f
	case err != nil:
		f.Status, f.Detail = ArtifactError, err.Error()
		return f
	case !fi.Mode().IsRegular():
		f.Status, f.Detail = ArtifactUnsafePath, "not a regular file (symlink or directory)"
		return f
	}
	f.ActualSize = fi.Size()
	if fi.Size() != ref.Size {
		f.Status = ArtifactSizeMismatch
		return f
	}
	if ref.SHA256 != "" {
		h := sha256.New()
		fh, err := os.Open(full)
		if err != nil {
			f.Status, f.Detail = ArtifactError, err.Error()
			return f
		}
		defer fh.Close()
		if _, err := io.Copy(h, fh); err != nil {
			f.Status, f.Detail = ArtifactError, err.Error()
			return f
		}
		if hex.EncodeToString(h.Sum(nil)) != ref.SHA256 {
			f.Status = ArtifactHashMismatch
			return f
		}
	}
	f.Status = ArtifactOK
	return f
}

// Reconcile checks every external artifact the snapshot record references. A
// legacy record with no Artifacts but a TranscriptPath is checked by existence
// only (no recorded size/hash to compare). Findings are reported, never acted on.
func (s *Store) Reconcile(id string) ([]ArtifactFinding, error) {
	snap, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	var out []ArtifactFinding
	for _, ref := range snap.Artifacts {
		out = append(out, ReconcileArtifact(s.dataDir, ref))
	}
	if len(snap.Artifacts) == 0 && snap.TranscriptPath != "" {
		f := ArtifactFinding{Ref: ArtifactRef{Kind: ArtifactTranscript, Path: snap.TranscriptPath}, Status: ArtifactOK}
		if fi, err := os.Stat(snap.TranscriptPath); err != nil {
			f.Status = ArtifactMissing
		} else {
			f.Ref.Size = fi.Size()
		}
		out = append(out, f)
	}
	return out, nil
}
