package stateclassifier

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/srjn45/warden/internal/agentbackend"
)

// FileTranscriptInspector reads only adapter-declared transcript files. It is
// evidence, not a completion oracle: a quiet or unreadable transcript returns
// no turn evidence and can never complete an agent by itself.
type FileTranscriptInspector struct {
	mu   sync.Mutex
	seen map[string]int
}

func NewFileTranscriptInspector() *FileTranscriptInspector {
	return &FileTranscriptInspector{seen: make(map[string]int)}
}

func (i *FileTranscriptInspector) InspectTranscript(ctx context.Context, workdir, sessionID string, b agentbackend.Backend) (TranscriptInfo, error) {
	if err := ctx.Err(); err != nil {
		return TranscriptInfo{}, err
	}
	if b == nil {
		return TranscriptInfo{}, nil
	}
	path, ok := b.TranscriptPath("", workdir, sessionID)
	if !ok || path == "" {
		return TranscriptInfo{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return TranscriptInfo{}, fmt.Errorf("open adapter transcript: %w", err)
	}
	defer f.Close()
	turns, err := b.ParseTranscript(f)
	if err != nil {
		return TranscriptInfo{}, fmt.Errorf("parse adapter transcript: %w", err)
	}
	info := TranscriptInfo{HasTurns: len(turns) > 0, TurnCount: len(turns)}
	key := b.ID() + "\x00" + workdir + "\x00" + sessionID
	i.mu.Lock()
	prior := i.seen[key]
	i.seen[key] = len(turns)
	i.mu.Unlock()
	if len(turns) > prior {
		// New records show an active turn until a later investigation observes
		// the stable assistant result. This avoids treating silence as progress.
		info.TurnActive = true
	} else if len(turns) > 0 && turns[len(turns)-1].Role == "assistant" {
		info.TurnEnded = true
	}
	return info, nil
}
