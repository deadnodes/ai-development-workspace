package transport

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
)

type backupService interface {
	CreateBackup(context.Context, string) (application.Backup, error)
	RestoreBackup(context.Context, string, string, string) (any, error)
}

func registerBackupRoutes(mux *http.ServeMux, service Service) {
	backup, ok := service.(backupService)
	if !ok {
		return
	}
	mux.HandleFunc("POST /api/backups/export", func(w http.ResponseWriter, r *http.Request) {
		result, err := backup.CreateBackup(r.Context(), defaultActor(r.Header.Get("X-RCP-Actor")))
		if err != nil {
			respond(w, nil, err)
			return
		}
		archive, err := base64.StdEncoding.DecodeString(result.ArchiveBase64)
		if err != nil {
			respond(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="release-control.rcp.json.gz"`)
		w.Header().Set("X-Backup-SHA256", result.SHA256)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("POST /api/backups/restore", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/gzip" {
			write(w, 415, map[string]string{"error": "Use Content-Type: application/gzip"})
			return
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, application.MaxBackupCompressed))
		if err != nil {
			write(w, 413, map[string]string{"error": "Backup exceeds compressed limit or cannot be read"})
			return
		}
		result, err := backup.RestoreBackup(r.Context(), defaultActor(r.Header.Get("X-RCP-Actor")), base64.StdEncoding.EncodeToString(b), r.Header.Get("X-Backup-SHA256"))
		respond(w, result, err)
	})
}
func registerBackupTools(server *mcp.Server, service Service) {
	backup, ok := service.(backupService)
	if !ok {
		return
	}
	mcp.AddTool(server, &mcp.Tool{Name: "create_backup", Description: "Export the complete instance database state and history as gzip/base64, with SHA-256. Contains private context and secret references, not secret values or external image/log bytes. For file transfer prefer POST /api/backups/export. Limit 8 MiB compressed / 64 MiB expanded."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Actor string `json:"actor,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		result, err := backup.CreateBackup(ctx, defaultActor(in.Actor))
		return nil, result, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "restore_backup", Description: "Restore a trusted full-instance gzip/base64 backup into an EMPTY instance only. Checks checksum/version/size, writes atomically, preserves history. Pending/running operations are cancelled with original evidence retained to prevent duplicate external actions. Does not copy external credentials or merge existing databases."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Actor         string `json:"actor,omitempty"`
		ArchiveBase64 string `json:"archive_base64"`
		SHA256        string `json:"sha256"`
	}) (*mcp.CallToolResult, any, error) {
		result, err := backup.RestoreBackup(ctx, defaultActor(in.Actor), in.ArchiveBase64, in.SHA256)
		return nil, result, err
	})
	// Explicit compact capability metadata lets agents avoid sending oversized blobs.
	mcp.AddTool(server, &mcp.Tool{Name: "get_backup_capabilities", Description: "Read backup format, limits and restore policy before an agent migration."}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"format": "rcp.json.gz", "version": 1, "max_compressed_bytes": application.MaxBackupCompressed, "max_expanded_bytes": application.MaxBackupExpanded, "restore_policy": "empty-instance-only", "active_operations": "cancel-with-history", "export_path": "/api/backups/export", "restore_path": "/api/backups/restore"}, nil
	})
}
