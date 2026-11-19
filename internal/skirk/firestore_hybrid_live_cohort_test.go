package skirk

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type fsHybridLiveCampaign struct {
	CampaignID   string `json:"campaign_id"`
	TimeoutLimit int    `json:"timeout_limit"`
	Workload     string `json:"workload,omitempty"`
	Arms         []struct {
		ArmID string `json:"arm_id"`
		Mode  string `json:"mode"`
	} `json:"arms"`
}
type fsHybridLiveCohort struct {
	mu          sync.Mutex
	file        *os.File
	active, arm string
	count       int
	armCount    int
	times       []time.Time
	finished    bool
}

func fsHybridLiveBeginCohort(settings fsHybridLiveSettings) (*fsHybridLiveCohort, error) {
	raw, err := fsHybridPrivateRead(settings.CampaignLedger, 32768)
	if err != nil {
		return nil, err
	}
	var campaign fsHybridLiveCampaign
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&campaign) != nil || dec.Decode(new(any)) != io.EOF || campaign.TimeoutLimit != 64 || len(campaign.CampaignID) < 1 || len(campaign.CampaignID) > 64 || !isSafeObjectSegment(campaign.CampaignID) || len(campaign.Arms) < 1 || len(campaign.Arms) > 4 {
		return nil, errFSHybridBounds
	}
	workload := settings.Workload
	if workload == "" {
		workload = "http_serial"
	}
	if campaign.Workload == "" {
		campaign.Workload = "http_serial"
	}
	if campaign.Workload != workload || workload != "http_serial" && workload != "tls_pipeline" {
		return nil, errFSHybridBounds
	}
	matched := false
	seen := make(map[string]bool)
	for _, arm := range campaign.Arms {
		if len(arm.ArmID) < 3 || len(arm.ArmID) > 64 || !isSafeObjectSegment(arm.ArmID) || seen[arm.ArmID] || arm.Mode != "drive" && arm.Mode != "hybrid" && arm.Mode != "drive_v2" {
			return nil, errFSHybridBounds
		}
		seen[arm.ArmID] = true
		if arm.ArmID == settings.ArmID && arm.Mode == settings.Mode {
			matched = true
		}
	}
	if !matched {
		return nil, errFSHybridBounds
	}
	active := settings.CampaignLedger + ".active"
	f, err := os.OpenFile(active, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errFSHybridBounds
	}
	locked := false
	defer func() {
		_ = f.Close()
		if !locked {
			_ = os.Remove(active)
		}
	}()
	if json.NewEncoder(f).Encode(map[string]any{"arm_id": settings.ArmID, "pid": os.Getpid(), "started_utc": time.Now().UTC().Format(time.RFC3339Nano)}) != nil || f.Sync() != nil {
		return nil, errFSHybridBounds
	}
	marker := settings.CampaignLedger + ".arm-" + settings.ArmID + ".started"
	consumed, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errFSHybridBounds
	}
	err = json.NewEncoder(consumed).Encode(map[string]any{"mode": settings.Mode, "pid": os.Getpid()})
	if err == nil {
		err = consumed.Sync()
	}
	_ = consumed.Close()
	if err != nil {
		return nil, errFSHybridBounds
	}
	path := settings.CampaignLedger + ".timeouts.jsonl"
	if info, e := os.Lstat(path); e == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
			return nil, errFSHybridBounds
		}
	} else if !os.IsNotExist(e) {
		return nil, errFSHybridBounds
	}
	journal, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return nil, errFSHybridBounds
	}
	prior, err := io.ReadAll(io.LimitReader(journal, 1<<20))
	if err != nil {
		journal.Close()
		return nil, errFSHybridBounds
	}
	count := 0
	for _, line := range bytes.Split(bytes.TrimSpace(prior), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var entry struct {
			ArmID string `json:"arm_id"`
			At    string `json:"at_utc"`
		}
		if json.Unmarshal(line, &entry) != nil || !seen[entry.ArmID] {
			journal.Close()
			return nil, errFSHybridBounds
		}
		count++
	}
	if count >= 64 {
		journal.Close()
		return nil, errFSHybridBounds
	}
	locked = true
	return &fsHybridLiveCohort{file: journal, active: active, arm: settings.ArmID, count: count}, nil
}
func (c *fsHybridLiveCohort) timeout() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return errFSHybridClosed
	}
	c.count++
	c.armCount++
	now := time.Now()
	var recent []time.Time
	for _, v := range c.times {
		if v.After(now.Add(-time.Minute)) {
			recent = append(recent, v)
		}
	}
	c.times = append(recent, now)
	if json.NewEncoder(c.file).Encode(map[string]any{"arm_id": c.arm, "at_utc": time.Now().UTC().Format(time.RFC3339Nano)}) != nil || c.file.Sync() != nil {
		return errFSHybridBounds
	}
	if c.count >= 64 || c.armCount >= 32 || len(c.times) >= 16 {
		return errFSHybridBounds
	}
	return nil
}
func (c *fsHybridLiveCohort) close(joined bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return nil
	}
	c.finished = true
	err := c.file.Sync()
	if e := c.file.Close(); err == nil {
		err = e
	}
	if joined && err == nil {
		err = os.Remove(c.active)
	}
	return err
}
func fsHybridLiveExclusiveJSON(path string, value any) error {
	if filepath.IsAbs(path) == false {
		return errFSHybridBounds
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errFSHybridBounds
	}
	defer f.Close()
	if json.NewEncoder(f).Encode(value) != nil || f.Sync() != nil {
		return errFSHybridBounds
	}
	return nil
}
