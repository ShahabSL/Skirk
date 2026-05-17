package skirk

import "testing"

func TestErrorSummaryGoogleAPIReasons(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "missing drive folder",
			err:  &GoogleAPIError{Op: "drive upload", Status: 404, Reason: "notFound"},
			want: "drive_not_found",
		},
		{
			name: "storage quota",
			err:  &GoogleAPIError{Op: "drive upload", Status: 403, Reason: "storageQuotaExceeded"},
			want: "storage_quota_exceeded",
		},
		{
			name: "unknown status",
			err:  &GoogleAPIError{Op: "drive upload", Status: 418},
			want: "drive_status_418",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorSummary(tt.err); got != tt.want {
				t.Fatalf("errorSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}
