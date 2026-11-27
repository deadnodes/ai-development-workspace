package persistence

import (
	"context"
	"encoding/json"

	"releasecontrol/internal/domain"
)

func (s *Store) ReadGitHubConnections(ctx context.Context) ([]domain.GitHubConnection, error) {
	rows, err := s.pool.Query(ctx, githubConnectionsSQL)
	return decodeGitHubConnections(rows, err)
}

func (s *SQLiteStore) ReadGitHubConnections(ctx context.Context) ([]domain.GitHubConnection, error) {
	rows, err := s.db.QueryContext(ctx, githubConnectionsSQL)
	return decodeGitHubConnections(sqliteRows{rows}, err)
}

const githubConnectionsSQL = `SELECT document FROM records WHERE kind='github_connections' ORDER BY position,id`

func decodeGitHubConnections(rows stateRows, err error) ([]domain.GitHubConnection, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var connections []domain.GitHubConnection
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var connection domain.GitHubConnection
		if err := json.Unmarshal(raw, &connection); err != nil {
			return nil, err
		}
		connections = append(connections, connection)
	}
	return connections, rows.Err()
}
