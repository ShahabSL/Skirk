package skirk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	driveMetadataSlotPrefix         = "slotv1/"
	driveMetadataSlotMaxNameBytes   = 255
	driveMetadataSlotMaxDescription = 8 * 1024
	driveMetadataSlotFields         = "id,name,description,version,parents,spaces,mimeType,size,ownedByMe,isAppAuthorized"
)

type driveMetadataSlotResponse struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Version         string   `json:"version"`
	Parents         []string `json:"parents"`
	Spaces          []string `json:"spaces"`
	MimeType        string   `json:"mimeType"`
	Size            string   `json:"size"`
	OwnedByMe       *bool    `json:"ownedByMe"`
	IsAppAuthorized *bool    `json:"isAppAuthorized"`
}

func (d *DriveStore) MetadataSlotEnabled() bool {
	return d != nil && d.metadataSlotV1 && (d.isAppData() || d.folderID != "")
}

func validMetadataSlotName(name string) bool {
	if !strings.HasPrefix(name, driveMetadataSlotPrefix) || len(name) <= len(driveMetadataSlotPrefix) || len(name) > driveMetadataSlotMaxNameBytes || strings.HasSuffix(name, "/") || strings.Contains(name, "//") || strings.Contains(name, "..") {
		return false
	}
	for _, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == '/' {
			continue
		}
		return false
	}
	return true
}

func validMetadataSlotID(id string) bool {
	if id == "" || len(id) > 512 || strings.TrimSpace(id) != id {
		return false
	}
	for _, ch := range id {
		if ch < 0x21 || ch == 0x7f {
			return false
		}
	}
	return true
}

func validMetadataSlotDescription(description string) bool {
	if len(description) > driveMetadataSlotMaxDescription || !utf8.ValidString(description) {
		return false
	}
	for _, ch := range description {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	return true
}

func (d *DriveStore) metadataSlotParent() (string, error) {
	if !d.MetadataSlotEnabled() {
		return "", errors.New("Drive metadata slots are disabled or mailbox is unset")
	}
	if d.isAppData() {
		return "appDataFolder", nil
	}
	return d.folderID, nil
}

// CreateMetadataSlot requires a generated ID so an indeterminate create can
// be checked and cleaned by exact ID. The returned ID remains set on error.
func (d *DriveStore) CreateMetadataSlot(ctx context.Context, generatedID, name string) (MetadataSlotInfo, error) {
	partial := MetadataSlotInfo{ID: generatedID, Name: name}
	parent, err := d.metadataSlotParent()
	if err != nil {
		return partial, err
	}
	if !validMetadataSlotID(generatedID) || !validMetadataSlotName(name) {
		return partial, errors.New("invalid Drive metadata slot ID or name")
	}
	body, err := json.Marshal(map[string]any{
		"id": generatedID, "name": name, "mimeType": "application/octet-stream",
		"parents": []string{parent}, "description": "",
	})
	if err != nil {
		return partial, err
	}
	result, err := d.request(ctx, http.MethodPost, "/drive/v3/files?fields="+url.QueryEscape(driveMetadataSlotFields), map[string]string{"Content-Type": "application/json; charset=UTF-8"}, body)
	if err != nil {
		return partial, err
	}
	if err := require2xx(result, "drive metadata slot create"); err != nil {
		return partial, err
	}
	info, err := d.parseMetadataSlot(result.Body, generatedID, name)
	if err != nil {
		return partial, err
	}
	d.rememberMetadataSlot(info.ID, info.Name)
	return info, nil
}

// GetMetadataSlot reads an exact known ID and validates its identity and
// mailbox location before the protocol accepts its opaque description.
func (d *DriveStore) GetMetadataSlot(ctx context.Context, fileID, expectedName string) (MetadataSlotInfo, error) {
	if _, err := d.metadataSlotParent(); err != nil {
		return MetadataSlotInfo{}, err
	}
	if !validMetadataSlotID(fileID) || !validMetadataSlotName(expectedName) {
		return MetadataSlotInfo{}, errors.New("invalid Drive metadata slot ID or name")
	}
	path := "/drive/v3/files/" + url.PathEscape(fileID) + "?fields=" + url.QueryEscape(driveMetadataSlotFields)
	result, err := d.request(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return MetadataSlotInfo{}, err
	}
	if err := require2xx(result, "drive metadata slot get"); err != nil {
		if result.Status == http.StatusNotFound {
			d.forgetMetadataSlot(fileID)
		}
		return MetadataSlotInfo{}, err
	}
	info, err := d.parseMetadataSlot(result.Body, fileID, expectedName)
	if err != nil {
		d.forgetMetadataSlot(fileID)
		return MetadataSlotInfo{}, err
	}
	d.rememberMetadataSlot(fileID, expectedName)
	return info, nil
}

// PatchMetadataSlot sends exactly one HTTP attempt. On a transport error the
// caller must read back the ID or close/fallback before publishing again.
// The protocol must serialize calls for each slot's single writer.
func (d *DriveStore) PatchMetadataSlot(ctx context.Context, fileID, expectedName, description string) (MetadataSlotInfo, error) {
	if _, err := d.metadataSlotParent(); err != nil {
		return MetadataSlotInfo{}, err
	}
	if !validMetadataSlotID(fileID) || !validMetadataSlotName(expectedName) || !d.knowsMetadataSlot(fileID, expectedName) {
		return MetadataSlotInfo{}, errors.New("Drive metadata slot must be validated before PATCH")
	}
	if description == "" || !validMetadataSlotDescription(description) {
		return MetadataSlotInfo{}, fmt.Errorf("Drive metadata slot description must contain 1-%d valid UTF-8 bytes", driveMetadataSlotMaxDescription)
	}
	body, err := json.Marshal(map[string]string{"description": description})
	if err != nil {
		return MetadataSlotInfo{}, err
	}
	path := "/drive/v3/files/" + url.PathEscape(fileID) + "?fields=" + url.QueryEscape(driveMetadataSlotFields)
	result, err := d.requestSlotPatch(ctx, path, map[string]string{"Content-Type": "application/json; charset=UTF-8"}, body)
	if err != nil {
		return MetadataSlotInfo{}, err
	}
	if err := require2xx(result, "drive metadata slot patch"); err != nil {
		if result.Status == http.StatusNotFound {
			d.forgetMetadataSlot(fileID)
		}
		return MetadataSlotInfo{}, err
	}
	info, err := d.parseMetadataSlot(result.Body, fileID, expectedName)
	if err != nil || info.Description != description {
		d.forgetMetadataSlot(fileID)
		if err != nil {
			return MetadataSlotInfo{}, err
		}
		return MetadataSlotInfo{}, errors.New("Drive metadata slot PATCH returned a different description")
	}
	return info, nil
}

func (d *DriveStore) parseMetadataSlot(body []byte, expectedID, expectedName string) (MetadataSlotInfo, error) {
	var file driveMetadataSlotResponse
	if err := json.Unmarshal(body, &file); err != nil {
		return MetadataSlotInfo{}, fmt.Errorf("decode Drive metadata slot: %w", err)
	}
	if file.ID != expectedID || file.Name != expectedName || file.MimeType != "application/octet-stream" {
		return MetadataSlotInfo{}, errors.New("Drive metadata slot identity or MIME type mismatch")
	}
	if !validMetadataSlotDescription(file.Description) {
		return MetadataSlotInfo{}, errors.New("Drive metadata slot description is invalid or oversized")
	}
	if file.OwnedByMe != nil && !*file.OwnedByMe || file.IsAppAuthorized != nil && !*file.IsAppAuthorized {
		return MetadataSlotInfo{}, errors.New("Drive metadata slot is not owned or app-authorized")
	}
	if file.Size != "" && file.Size != "0" {
		return MetadataSlotInfo{}, errors.New("Drive metadata slot has unexpected media content")
	}
	if d.isAppData() {
		if len(file.Spaces) != 1 || file.Spaces[0] != "appDataFolder" || len(file.Parents) != 1 || file.Parents[0] == "" {
			return MetadataSlotInfo{}, errors.New("Drive metadata slot is outside appDataFolder")
		}
	} else if len(file.Parents) != 1 || file.Parents[0] != d.folderID || len(file.Spaces) != 1 || file.Spaces[0] != "drive" {
		return MetadataSlotInfo{}, errors.New("Drive metadata slot is outside configured mailbox folder")
	}
	version, err := strconv.ParseUint(file.Version, 10, 64)
	if err != nil || version == 0 {
		return MetadataSlotInfo{}, errors.New("Drive metadata slot has invalid version")
	}
	return MetadataSlotInfo{ID: file.ID, Name: file.Name, Description: file.Description, Version: version, Parents: file.Parents, Spaces: file.Spaces}, nil
}

func (d *DriveStore) rememberMetadataSlot(id, name string) {
	d.metadataSlotMu.Lock()
	defer d.metadataSlotMu.Unlock()
	if d.metadataSlotNames == nil {
		d.metadataSlotNames = make(map[string]string)
	}
	d.metadataSlotNames[id] = name
}

func (d *DriveStore) knowsMetadataSlot(id, name string) bool {
	d.metadataSlotMu.Lock()
	defer d.metadataSlotMu.Unlock()
	return d.metadataSlotNames[id] == name
}

func (d *DriveStore) forgetMetadataSlot(id string) {
	d.metadataSlotMu.Lock()
	defer d.metadataSlotMu.Unlock()
	delete(d.metadataSlotNames, id)
}
