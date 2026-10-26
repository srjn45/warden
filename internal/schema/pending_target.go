package schema

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PendingSchema3TargetAuthorized narrowly permits the sealed target executable
// to boot during readiness; ordinary interrupted ledgers remain refused.
func PendingSchema3TargetAuthorized(dataDir string, l *Ledger) error {
	if l == nil || l.SchemaVersion != 3 || l.InProgress == nil {
		return fmt.Errorf("schema3 pending target: no transaction")
	}
	ip := l.InProgress
	if ip.Migration != "0003-ownership-normalization" || !safeID(ip.TransactionID) || ip.SourceFormat != 2 || ip.TargetFormat != 3 || ip.Step != "ledger_committed" || ip.ManifestSHA256 == "" || ip.TargetBinarySHA256 == "" {
		return fmt.Errorf("schema3 pending target: invalid ledger authorization")
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return fmt.Errorf("schema3 pending target: canonical data dir: %w", err)
	}
	manifestPath := filepath.Join(root, "backups", "pre-schema3-"+ip.TransactionID, "MANIFEST.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	if digest(raw) != ip.ManifestSHA256 {
		return fmt.Errorf("schema3 pending target: sealed manifest hash mismatch")
	}
	var m struct {
		Format      int    `json:"format"`
		Transaction string `json:"transaction"`
		Migration   string `json:"migration"`
		Policy      string `json:"policy"`
		From        int    `json:"source_format"`
		To          int    `json:"target_format"`
		Capability  int    `json:"capability_version"`
		DataDir     string `json:"data_dir"`
		Target      string `json:"target_binary_sha256"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Format != 1 || m.Transaction != ip.TransactionID || m.Migration != ip.Migration || m.Policy != "ownership-v1" || m.From != 2 || m.To != 3 || m.Capability != 1 || m.DataDir != root || m.Target != ip.TargetBinarySHA256 {
		return fmt.Errorf("schema3 pending target: sealed manifest identity mismatch")
	}
	journal := filepath.Join(root, ".schema3-transaction-"+ip.TransactionID+".jsonl")
	if ip.Journal != journal {
		return fmt.Errorf("schema3 pending target: journal path mismatch")
	}
	f, err := os.Open(journal)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("schema3 pending target: unsafe journal")
	}
	last := []byte{0}
	if _, err := f.ReadAt(last, info.Size()-1); err != nil || last[0] != '\n' {
		return fmt.Errorf("schema3 pending target: unterminated journal")
	}
	committed, restore, seq, prev := false, false, 0, ""
	lastStep, lastComplete := "", false
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() {
		var r struct {
			Seq         int    `json:"seq"`
			Previous    string `json:"previous"`
			Manifest    string `json:"manifest_hash"`
			Transaction string `json:"transaction"`
			Step        string `json:"step"`
			Complete    bool   `json:"complete"`
			Source      string `json:"source"`
			Staging     string `json:"staging"`
			Parked      string `json:"parked"`
			Target      string `json:"target_binary_sha256"`
			Input       string `json:"input"`
			Output      string `json:"output"`
		}
		if json.Unmarshal(s.Bytes(), &r) != nil || r.Seq != seq+1 || r.Previous != prev || r.Manifest != ip.ManifestSHA256 || r.Transaction != ip.TransactionID || r.Source != filepath.Join(root, WardenDBDir) || r.Staging != filepath.Join(root, ".schema3-staging-"+ip.TransactionID) || r.Parked != filepath.Join(root, ".schema2-parked-"+ip.TransactionID) || r.Target != ip.TargetBinarySHA256 {
			return fmt.Errorf("schema3 pending target: journal identity mismatch")
		}
		seq++
		prev = digestAny(r)
		committed = committed || (r.Step == "ledger_committed" && r.Complete)
		restore = restore || r.Step == "restore_staged"
		lastStep, lastComplete = r.Step, r.Complete
	}
	if s.Err() != nil || !committed || restore || lastStep != "ledger_committed" || !lastComplete {
		return fmt.Errorf("schema3 pending target: journal not awaiting readiness")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(exe)
	if err != nil || digest(body) != ip.TargetBinarySHA256 {
		return fmt.Errorf("schema3 pending target: executable hash mismatch")
	}
	return nil
}

func safeID(id string) bool    { return id != "" && !strings.ContainsAny(id, "/\\.") }
func digest(raw []byte) string { s := sha256.Sum256(raw); return "sha256:" + hex.EncodeToString(s[:]) }
func digestAny(v any) string   { raw, _ := json.Marshal(v); return digest(raw) }
