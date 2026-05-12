package skirk

import (
	"context"
	"log"
)

func StoresFromConfig(ctx context.Context, cfg *Config) (*DriveStore, error) {
	tokenSource := NewAccessTokenSource(cfg.Auth, cfg.Route)
	tokenSource.Logger = log.Default()
	if _, err := tokenSource.Token(ctx); err != nil {
		return nil, err
	}
	httpClient := NewGoogleHTTPClient(cfg.Route)
	drive := NewDriveStoreWithTokenSource(httpClient, tokenSource, cfg.Drive)
	drive.Logger = log.Default()
	return drive, nil
}
