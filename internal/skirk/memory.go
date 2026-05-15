package skirk

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type MemoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	ids     map[string]string
	nextID  uint64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{objects: map[string][]byte{}, ids: map[string]string{}}
}

func (s *MemoryStore) Put(_ context.Context, name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[name] = append([]byte(nil), data...)
	return nil
}

func (s *MemoryStore) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	if err := s.Put(ctx, name, data); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Name: name, Size: int64(len(data))}, nil
}

func (s *MemoryStore) PutObjectWithID(_ context.Context, fileID, name string, data []byte) (ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fileID == "" {
		return ObjectInfo{}, fmt.Errorf("object id is required")
	}
	if existingName, ok := s.ids[fileID]; ok {
		if existingName != name {
			return ObjectInfo{}, fmt.Errorf("object id conflict name=%q want=%q", existingName, name)
		}
		existing, ok := s.objects[name]
		if !ok {
			return ObjectInfo{}, fmt.Errorf("object id conflict missing object: %s", fileID)
		}
		if !bytes.Equal(existing, data) {
			return ObjectInfo{}, fmt.Errorf("object id conflict data mismatch: %s", fileID)
		}
		return ObjectInfo{Name: name, ID: fileID, Size: int64(len(existing))}, nil
	}
	if existingID := s.objectIDLocked(name); existingID != "" && existingID != fileID {
		return ObjectInfo{}, fmt.Errorf("object name conflict id=%q want=%q", existingID, fileID)
	}
	s.objects[name] = append([]byte(nil), data...)
	s.ids[fileID] = name
	return ObjectInfo{Name: name, ID: fileID, Size: int64(len(data))}, nil
}

func (s *MemoryStore) GenerateObjectIDs(_ context.Context, count int) ([]string, error) {
	if count < 0 {
		return nil, fmt.Errorf("object id count must be non-negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		s.nextID++
		ids = append(ids, fmt.Sprintf("mem-id-%016x", s.nextID))
	}
	return ids, nil
}

func (s *MemoryStore) Get(_ context.Context, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[name]
	if !ok {
		return nil, fmt.Errorf("object not found: %s", name)
	}
	return append([]byte(nil), data...), nil
}

func (s *MemoryStore) GetByID(_ context.Context, fileID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.ids[fileID]
	if !ok {
		return nil, fmt.Errorf("object id not found: %s", fileID)
	}
	data, ok := s.objects[name]
	if !ok {
		return nil, fmt.Errorf("object not found for id: %s", fileID)
	}
	return append([]byte(nil), data...), nil
}

func (s *MemoryStore) GetObjectRangeByID(_ context.Context, fileID string, start, end int64) ([]byte, ObjectRangeInfo, error) {
	if start < 0 || end < start {
		return nil, ObjectRangeInfo{}, fmt.Errorf("invalid byte range %d-%d", start, end)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.ids[fileID]
	if !ok {
		return nil, ObjectRangeInfo{}, fmt.Errorf("object id not found: %s", fileID)
	}
	data, ok := s.objects[name]
	if !ok {
		return nil, ObjectRangeInfo{}, fmt.Errorf("object not found for id: %s", fileID)
	}
	if end >= int64(len(data)) {
		return nil, ObjectRangeInfo{}, fmt.Errorf("range %d-%d exceeds object size %d", start, end, len(data))
	}
	body := append([]byte(nil), data[start:end+1]...)
	return body, ObjectRangeInfo{Start: start, End: end, Total: int64(len(data))}, nil
}

func (s *MemoryStore) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var infos []ObjectInfo
	for name, data := range s.objects {
		if strings.HasPrefix(name, prefix) {
			infos = append(infos, ObjectInfo{Name: name, ID: s.objectIDLocked(name), Size: int64(len(data))})
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

func (s *MemoryStore) objectIDLocked(name string) string {
	for id, objectName := range s.ids {
		if objectName == name {
			return id
		}
	}
	return ""
}

func (s *MemoryStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, name)
	for id, objectName := range s.ids {
		if objectName == name {
			delete(s.ids, id)
		}
	}
	return nil
}

func (s *MemoryStore) DeleteID(_ context.Context, fileID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.ids[fileID]
	if ok {
		delete(s.objects, name)
		delete(s.ids, fileID)
	}
	return nil
}
