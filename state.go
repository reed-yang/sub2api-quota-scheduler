package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const stateFile = "state.json"
const decisionsFile = "decisions.jsonl"

// LoadState reads the persisted state; a missing file yields an empty state.
func LoadState(dir string) (State, error) {
	s := State{DisabledUntil: map[int64]int64{}}
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if s.DisabledUntil == nil {
		s.DisabledUntil = map[int64]int64{}
	}
	return s, nil
}

// SaveState writes the state atomically.
func SaveState(dir string, s State) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, stateFile))
}

// AppendDecision appends one JSON line to the decision log.
func AppendDecision(dir string, d Decision) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, decisionsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}
