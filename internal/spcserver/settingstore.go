package spcserver

import (
	"context"
	"database/sql"

	"github.com/sysop/ultrabridge/internal/notedb"
)

// settingStore adapts the shared notedb settings table to the small Get/Set
// interface the auth middleware and login resolver consume (auth.SettingStore).
// It is the production implementation; tests use fakes.
type settingStore struct{ db *sql.DB }

func (s settingStore) Get(ctx context.Context, key string) (string, error) {
	return notedb.GetSetting(ctx, s.db, key)
}

func (s settingStore) Set(ctx context.Context, key, val string) error {
	return notedb.SetSetting(ctx, s.db, key, val)
}
