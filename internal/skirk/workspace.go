package skirk

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
)

type Workspace struct {
	http  *GoogleHTTPClient
	token string
}

func NewWorkspace(httpClient *GoogleHTTPClient, token string) *Workspace {
	return &Workspace{http: httpClient, token: token}
}

func (w *Workspace) CreateSpreadsheet(ctx context.Context, title, sheetTitle string) (string, error) {
	if title == "" {
		title = "skirk-workspace"
	}
	if sheetTitle == "" {
		sheetTitle = "skirk"
	}
	body, err := json.Marshal(map[string]any{
		"properties": map[string]string{"title": title},
		"sheets": []map[string]any{
			{"properties": map[string]string{"title": sheetTitle}},
		},
	})
	if err != nil {
		return "", err
	}
	result, err := w.http.Request(ctx, http.MethodPost, "sheets.googleapis.com", "/v4/spreadsheets", w.jsonHeaders(), body)
	if err != nil {
		return "", err
	}
	if err := require2xx(result, "sheets create"); err != nil {
		return "", err
	}
	var payload struct {
		SpreadsheetID string `json:"spreadsheetId"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return "", err
	}
	if payload.SpreadsheetID == "" {
		return "", fmt.Errorf("sheets create response did not include spreadsheetId")
	}
	return payload.SpreadsheetID, nil
}

func (w *Workspace) CreateDriveFolder(ctx context.Context, name string) (string, error) {
	if name == "" {
		name = "skirk-data"
	}
	body, err := json.Marshal(map[string]string{
		"name":     name,
		"mimeType": "application/vnd.google-apps.folder",
	})
	if err != nil {
		return "", err
	}
	result, err := w.http.Request(ctx, http.MethodPost, "www.googleapis.com", "/drive/v3/files?fields=id,name", w.jsonHeaders(), body)
	if err != nil {
		return "", err
	}
	if err := require2xx(result, "drive folder create"); err != nil {
		return "", err
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result.Body, &payload); err != nil {
		return "", err
	}
	if payload.ID == "" {
		return "", fmt.Errorf("drive folder create response did not include id")
	}
	return payload.ID, nil
}

func (w *Workspace) DeleteSpreadsheet(ctx context.Context, spreadsheetID string) error {
	return w.DeleteDriveFile(ctx, spreadsheetID)
}

func (w *Workspace) DeleteDriveFile(ctx context.Context, fileID string) error {
	if fileID == "" {
		return nil
	}
	result, err := w.http.Request(ctx, http.MethodDelete, "www.googleapis.com", "/drive/v3/files/"+url.PathEscape(fileID), w.authHeaders(), nil)
	if err != nil {
		return err
	}
	if result.Status == http.StatusNoContent || result.Status == http.StatusOK || result.Status == http.StatusNotFound {
		return nil
	}
	return require2xx(result, "drive file delete")
}

func (w *Workspace) jsonHeaders() map[string]string {
	headers := w.authHeaders()
	headers["Content-Type"] = "application/json"
	return headers
}

func (w *Workspace) authHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + w.token}
}

func StoresFromConfig(ctx context.Context, cfg *Config) (*DriveStore, *SheetsLog, *Workspace, error) {
	tokenSource := NewAccessTokenSource(cfg.Auth, cfg.Route)
	tokenSource.Logger = log.Default()
	token, err := tokenSource.Token(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	httpClient := NewGoogleHTTPClient(cfg.Route)
	drive := NewDriveStoreWithTokenSource(httpClient, tokenSource, cfg.Drive)
	drive.Logger = log.Default()
	sheets := NewSheetsLog(httpClient, token, cfg.Sheets)
	workspace := NewWorkspace(httpClient, token)
	return drive, sheets, workspace, nil
}
