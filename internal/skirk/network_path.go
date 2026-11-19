package skirk

// RefreshNetworkPath retires idle carrier connections without canceling HTTP
// work, closing local listeners, or resetting the peer's mux state. New dials
// use the operating system's current default route.
func (t *Tunnel) RefreshNetworkPath() {
	clients := make(map[*GoogleHTTPClient]struct{})
	if drive, ok := t.Data.(*DriveStore); ok {
		for _, client := range []*GoogleHTTPClient{drive.http, drive.normalUploadHTTP, drive.normalDownloadHTTP, drive.slotReadHTTP, drive.slotWriteHTTP} {
			clients[client] = struct{}{}
		}
	}
	if preview := t.udpPreview; preview != nil {
		clients[preview.http] = struct{}{}
		preview.metricsMu.RLock()
		for mailbox := range preview.metricsMailboxes {
			if uploads := mailbox.adaptiveUploads; uploads != nil {
				clients[uploads.mediaHTTP.Load()] = struct{}{}
			}
		}
		preview.metricsMu.RUnlock()
	}
	for client := range clients {
		if client != nil && client.client != nil {
			client.client.CloseIdleConnections()
		}
	}
}

func (t *googleH3FallbackTransport) CloseIdleConnections() {
	if h3, ok := t.h3.(interface{ CloseIdleConnections() }); ok {
		h3.CloseIdleConnections()
	}
	if h2, ok := t.h2.(interface{ CloseIdleConnections() }); ok {
		h2.CloseIdleConnections()
	}
}
