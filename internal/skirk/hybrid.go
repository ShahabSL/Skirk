package skirk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

const (
	controlPrefix = "control"
	dataPrefix    = "data"
)

type HybridSendResult struct {
	SessionID      string   `json:"session_id"`
	Chunks         int      `json:"chunks"`
	BytesPlaintext int64    `json:"bytes_plaintext"`
	DriveObjects   []string `json:"drive_objects"`
	DriveFileIDs   []string `json:"drive_file_ids,omitempty"`
	ControlRows    []string `json:"control_rows"`
}

type HybridReceiveResult struct {
	SessionID      string   `json:"session_id"`
	Chunks         int      `json:"chunks"`
	BytesPlaintext int64    `json:"bytes_plaintext"`
	DriveObjects   []string `json:"drive_objects"`
	DriveFileIDs   []string `json:"drive_file_ids,omitempty"`
	ControlRows    []string `json:"control_rows"`
}

type ControlPayload struct {
	Version     int              `json:"v"`
	Event       string           `json:"event"`
	SessionID   string           `json:"session_id"`
	ConnID      string           `json:"conn_id,omitempty"`
	Direction   string           `json:"direction"`
	Sequence    uint64           `json:"sequence"`
	Batch       []ControlPayload `json:"batch,omitempty"`
	DriveObject string           `json:"drive_object,omitempty"`
	DriveFileID string           `json:"drive_file_id,omitempty"`
	ControlFile bool             `json:"control_file,omitempty"`
	InlineData  string           `json:"inline_data,omitempty"`
	Target      string           `json:"target,omitempty"`
	Bytes       int              `json:"bytes,omitempty"`
	Final       bool             `json:"final,omitempty"`
	Error       string           `json:"error,omitempty"`
}

func HybridSendFile(ctx context.Context, data BlobStore, control BlobStore, inputPath, secret, sessionID string, direction byte, chunkSize int, cleanupExisting bool) (HybridSendResult, error) {
	if chunkSize <= 0 {
		chunkSize = 8192
	}
	sid, err := ParseSessionID(sessionID)
	if err != nil {
		return HybridSendResult{}, err
	}
	key, err := DeriveKey(secret)
	if err != nil {
		return HybridSendResult{}, err
	}
	if cleanupExisting {
		infos, err := control.List(ctx, fileControlPrefix(sid, direction))
		if err != nil {
			return HybridSendResult{}, err
		}
		for _, info := range infos {
			_ = control.Delete(ctx, info.Name)
		}
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return HybridSendResult{}, err
	}
	defer input.Close()
	stat, err := input.Stat()
	if err != nil {
		return HybridSendResult{}, err
	}

	var result HybridSendResult
	result.SessionID = SessionString(sid)
	buffer := make([]byte, chunkSize)
	if stat.Size() == 0 {
		if err := sendHybridChunk(ctx, data, control, key, sid, direction, 0, nil, true, &result); err != nil {
			return HybridSendResult{}, err
		}
		return result, nil
	}
	remaining := stat.Size()
	for {
		n, readErr := input.Read(buffer)
		if n == 0 && readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != io.EOF {
			return HybridSendResult{}, readErr
		}
		chunk := buffer[:n]
		remaining -= int64(n)
		final := remaining == 0
		if err := sendHybridChunk(ctx, data, control, key, sid, direction, uint64(result.Chunks), chunk, final, &result); err != nil {
			return HybridSendResult{}, err
		}
		if final {
			break
		}
	}
	return result, nil
}

func sendHybridChunk(ctx context.Context, data BlobStore, control BlobStore, key []byte, sid [16]byte, direction byte, sequence uint64, chunk []byte, final bool, result *HybridSendResult) error {
	dataName := fileDataName(sid, direction, sequence)
	sealed, err := Seal(key, sid, direction, sequence, chunk, final)
	if err != nil {
		return err
	}
	fileID := ""
	if drive, ok := data.(*DriveStore); ok {
		info, err := drive.PutObject(ctx, dataName, sealed)
		if err != nil {
			return err
		}
		fileID = info.ID
	} else {
		if err := data.Put(ctx, dataName, sealed); err != nil {
			return err
		}
	}
	controlName := fileControlName(sid, direction, sequence)
	payload := ControlPayload{
		Version:     1,
		Event:       "CHUNK_READY",
		SessionID:   SessionString(sid),
		Direction:   directionName(direction),
		Sequence:    sequence,
		DriveObject: dataName,
		DriveFileID: fileID,
		Bytes:       len(chunk),
		Final:       final,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := control.Put(ctx, controlName, payloadBytes); err != nil {
		return err
	}
	result.DriveObjects = append(result.DriveObjects, dataName)
	if fileID != "" {
		result.DriveFileIDs = append(result.DriveFileIDs, fileID)
	}
	result.ControlRows = append(result.ControlRows, controlName)
	result.BytesPlaintext += int64(len(chunk))
	result.Chunks++
	return nil
}

type HybridBulkOptions struct {
	ChunkSize   int
	Concurrency int
}

func HybridSendFileBulk(ctx context.Context, drive *DriveStore, sheets *SheetsLog, inputPath, secret, sessionID string, direction byte, opts HybridBulkOptions) (HybridSendResult, error) {
	chunkSize := opts.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 1024 * 1024
	}
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sid, err := ParseSessionID(sessionID)
	if err != nil {
		return HybridSendResult{}, err
	}
	key, err := DeriveKey(secret)
	if err != nil {
		return HybridSendResult{}, err
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return HybridSendResult{}, err
	}
	defer input.Close()
	stat, err := input.Stat()
	if err != nil {
		return HybridSendResult{}, err
	}
	chunkCount := int((stat.Size() + int64(chunkSize) - 1) / int64(chunkSize))
	if chunkCount == 0 {
		chunkCount = 1
	}
	type uploadJob struct {
		seq   int
		data  []byte
		final bool
	}
	type uploadResult struct {
		seq         int
		controlName string
		dataName    string
		fileID      string
		bytes       int
		payload     []byte
		err         error
	}
	jobs := make(chan uploadJob)
	results := make([]uploadResult, chunkCount)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				seq := uint64(job.seq)
				dataName := fileDataName(sid, direction, seq)
				sealed, err := Seal(key, sid, direction, seq, job.data, job.final)
				if err != nil {
					results[job.seq] = uploadResult{seq: job.seq, err: err}
					continue
				}
				info, err := drive.PutObject(ctx, dataName, sealed)
				if err != nil {
					results[job.seq] = uploadResult{seq: job.seq, err: err}
					continue
				}
				controlName := fileControlName(sid, direction, seq)
				payload := ControlPayload{
					Version:     1,
					Event:       "CHUNK_READY",
					SessionID:   SessionString(sid),
					Direction:   directionName(direction),
					Sequence:    seq,
					DriveObject: dataName,
					DriveFileID: info.ID,
					Bytes:       len(job.data),
					Final:       job.final,
				}
				payloadBytes, err := json.Marshal(payload)
				if err != nil {
					results[job.seq] = uploadResult{seq: job.seq, err: err}
					continue
				}
				results[job.seq] = uploadResult{
					seq:         job.seq,
					controlName: controlName,
					dataName:    dataName,
					fileID:      info.ID,
					bytes:       len(job.data),
					payload:     payloadBytes,
				}
			}
		}()
	}
	buffer := make([]byte, chunkSize)
	seq := 0
	if stat.Size() == 0 {
		jobs <- uploadJob{seq: 0, data: nil, final: true}
	} else {
		remaining := stat.Size()
		for {
			n, readErr := input.Read(buffer)
			if n == 0 && readErr == io.EOF {
				break
			}
			if readErr != nil && readErr != io.EOF {
				close(jobs)
				wg.Wait()
				return HybridSendResult{}, readErr
			}
			chunk := append([]byte(nil), buffer[:n]...)
			remaining -= int64(n)
			jobs <- uploadJob{seq: seq, data: chunk, final: remaining == 0}
			seq++
			if remaining == 0 {
				break
			}
		}
	}
	close(jobs)
	wg.Wait()

	result := HybridSendResult{SessionID: SessionString(sid)}
	records := make([]SheetRecord, 0, chunkCount)
	var uploadedIDs []string
	for _, item := range results {
		if item.err != nil {
			_ = drive.DeleteIDs(context.Background(), uploadedIDs, concurrency)
			return HybridSendResult{}, item.err
		}
		result.DriveObjects = append(result.DriveObjects, item.dataName)
		result.DriveFileIDs = append(result.DriveFileIDs, item.fileID)
		result.ControlRows = append(result.ControlRows, item.controlName)
		result.BytesPlaintext += int64(item.bytes)
		result.Chunks++
		uploadedIDs = append(uploadedIDs, item.fileID)
		records = append(records, SheetRecord{Name: item.controlName, Data: item.payload, Action: "put"})
	}
	if err := sheets.PutMany(ctx, records); err != nil {
		_ = drive.DeleteIDs(context.Background(), uploadedIDs, concurrency)
		return HybridSendResult{}, err
	}
	return result, nil
}

func HybridReceiveFile(ctx context.Context, data BlobStore, control BlobStore, outputPath, secret, sessionID string, direction byte, deleteAfter bool) (HybridReceiveResult, error) {
	sid, err := ParseSessionID(sessionID)
	if err != nil {
		return HybridReceiveResult{}, err
	}
	key, err := DeriveKey(secret)
	if err != nil {
		return HybridReceiveResult{}, err
	}
	infos, err := control.List(ctx, fileControlPrefix(sid, direction))
	if err != nil {
		return HybridReceiveResult{}, err
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	if len(infos) == 0 {
		return HybridReceiveResult{}, fmt.Errorf("no control rows for session %s", sessionID)
	}
	output, err := os.Create(outputPath)
	if err != nil {
		return HybridReceiveResult{}, err
	}
	defer output.Close()

	result := HybridReceiveResult{SessionID: SessionString(sid)}
	expected := uint64(0)
	for _, info := range infos {
		raw, err := control.Get(ctx, info.Name)
		if err != nil {
			return HybridReceiveResult{}, err
		}
		var payload ControlPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			return HybridReceiveResult{}, err
		}
		if payload.Sequence != expected {
			return HybridReceiveResult{}, fmt.Errorf("missing sequence %d; got %d", expected, payload.Sequence)
		}
		var sealed []byte
		if payload.DriveFileID != "" {
			if drive, ok := data.(*DriveStore); ok {
				sealed, err = drive.GetByID(ctx, payload.DriveFileID)
			} else {
				sealed, err = data.Get(ctx, payload.DriveObject)
			}
		} else {
			sealed, err = data.Get(ctx, payload.DriveObject)
		}
		if err != nil {
			return HybridReceiveResult{}, err
		}
		env, plaintext, err := OpenEnvelope(key, sealed)
		if err != nil {
			return HybridReceiveResult{}, err
		}
		if env.SessionID != sid || env.Direction != direction || env.Sequence != expected {
			return HybridReceiveResult{}, fmt.Errorf("envelope metadata mismatch for %s", payload.DriveObject)
		}
		if _, err := output.Write(plaintext); err != nil {
			return HybridReceiveResult{}, err
		}
		result.DriveObjects = append(result.DriveObjects, payload.DriveObject)
		if payload.DriveFileID != "" {
			result.DriveFileIDs = append(result.DriveFileIDs, payload.DriveFileID)
		}
		result.ControlRows = append(result.ControlRows, info.Name)
		result.BytesPlaintext += int64(len(plaintext))
		result.Chunks++
		expected++
		if payload.Final {
			break
		}
	}
	if deleteAfter {
		for _, name := range result.DriveObjects {
			_ = data.Delete(ctx, name)
		}
		for _, name := range result.ControlRows {
			_ = control.Delete(ctx, name)
		}
	}
	return result, nil
}

func HybridReceiveFileBulk(ctx context.Context, drive *DriveStore, sheets *SheetsLog, outputPath, secret, sessionID string, direction byte, opts HybridBulkOptions) (HybridReceiveResult, error) {
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sid, err := ParseSessionID(sessionID)
	if err != nil {
		return HybridReceiveResult{}, err
	}
	key, err := DeriveKey(secret)
	if err != nil {
		return HybridReceiveResult{}, err
	}
	records, err := sheets.Entries(ctx, fileControlPrefix(sid, direction))
	if err != nil {
		return HybridReceiveResult{}, err
	}
	if len(records) == 0 {
		return HybridReceiveResult{}, fmt.Errorf("no control rows for session %s", sessionID)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	payloads := make([]ControlPayload, 0, len(records))
	for expected, record := range records {
		var payload ControlPayload
		if err := json.Unmarshal(record.Data, &payload); err != nil {
			return HybridReceiveResult{}, err
		}
		if payload.Sequence != uint64(expected) {
			return HybridReceiveResult{}, fmt.Errorf("missing sequence %d; got %d", expected, payload.Sequence)
		}
		payloads = append(payloads, payload)
		if payload.Final {
			break
		}
	}
	type downloadJob struct {
		index   int
		payload ControlPayload
	}
	type downloadResult struct {
		index     int
		plaintext []byte
		err       error
	}
	jobs := make(chan downloadJob)
	results := make([]downloadResult, len(payloads))
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				var sealed []byte
				var err error
				if job.payload.DriveFileID != "" {
					sealed, err = drive.GetByID(ctx, job.payload.DriveFileID)
				} else {
					sealed, err = drive.Get(ctx, job.payload.DriveObject)
				}
				if err != nil {
					results[job.index] = downloadResult{index: job.index, err: err}
					continue
				}
				env, plaintext, err := OpenEnvelope(key, sealed)
				if err != nil {
					results[job.index] = downloadResult{index: job.index, err: err}
					continue
				}
				if env.SessionID != sid || env.Direction != direction || env.Sequence != uint64(job.index) {
					results[job.index] = downloadResult{index: job.index, err: fmt.Errorf("envelope metadata mismatch for %s", job.payload.DriveObject)}
					continue
				}
				results[job.index] = downloadResult{index: job.index, plaintext: plaintext}
			}
		}()
	}
	for i, payload := range payloads {
		jobs <- downloadJob{index: i, payload: payload}
	}
	close(jobs)
	wg.Wait()

	output, err := os.Create(outputPath)
	if err != nil {
		return HybridReceiveResult{}, err
	}
	defer output.Close()
	result := HybridReceiveResult{SessionID: SessionString(sid)}
	for i, item := range results {
		if item.err != nil {
			return HybridReceiveResult{}, item.err
		}
		if _, err := output.Write(item.plaintext); err != nil {
			return HybridReceiveResult{}, err
		}
		payload := payloads[i]
		result.DriveObjects = append(result.DriveObjects, payload.DriveObject)
		if payload.DriveFileID != "" {
			result.DriveFileIDs = append(result.DriveFileIDs, payload.DriveFileID)
		}
		result.ControlRows = append(result.ControlRows, fileControlName(sid, direction, uint64(i)))
		result.BytesPlaintext += int64(len(item.plaintext))
		result.Chunks++
	}
	return result, nil
}

func fileDataName(sid [16]byte, direction byte, sequence uint64) string {
	return fmt.Sprintf("%s/%s/%s/%016x.skb", dataPrefix, SessionString(sid), directionName(direction), sequence)
}

func fileControlName(sid [16]byte, direction byte, sequence uint64) string {
	return fmt.Sprintf("%s/%s/%s/%016x", controlPrefix, SessionString(sid), directionName(direction), sequence)
}

func fileControlPrefix(sid [16]byte, direction byte) string {
	return fmt.Sprintf("%s/%s/%s/", controlPrefix, SessionString(sid), directionName(direction))
}

func directionName(direction byte) string {
	if direction == DirectionDown {
		return "down"
	}
	return "up"
}
