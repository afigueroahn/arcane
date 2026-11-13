package migrations

import (
	"github.com/pressly/goose/v3"
)

const (
	v114SQLiteUp = `
ALTER TABLE gitops_syncs ADD COLUMN build_strategy TEXT NOT NULL DEFAULT 'compose';
ALTER TABLE gitops_syncs ADD COLUMN source_path TEXT NOT NULL DEFAULT '';
ALTER TABLE gitops_syncs ADD COLUMN build_command TEXT NOT NULL DEFAULT '';
ALTER TABLE gitops_syncs ADD COLUMN start_command TEXT NOT NULL DEFAULT '';
ALTER TABLE gitops_syncs ADD COLUMN app_port INTEGER NOT NULL DEFAULT 0;
ALTER TABLE gitops_syncs ADD COLUMN host_port INTEGER NOT NULL DEFAULT 0;
ALTER TABLE gitops_syncs ADD COLUMN railpack_info TEXT;
`
	v114PostgresUp = `
ALTER TABLE gitops_syncs ADD COLUMN IF NOT EXISTS build_strategy text NOT NULL DEFAULT 'compose';
ALTER TABLE gitops_syncs ADD COLUMN IF NOT EXISTS source_path text NOT NULL DEFAULT '';
ALTER TABLE gitops_syncs ADD COLUMN IF NOT EXISTS build_command text NOT NULL DEFAULT '';
ALTER TABLE gitops_syncs ADD COLUMN IF NOT EXISTS start_command text NOT NULL DEFAULT '';
ALTER TABLE gitops_syncs ADD COLUMN IF NOT EXISTS app_port integer NOT NULL DEFAULT 0;
ALTER TABLE gitops_syncs ADD COLUMN IF NOT EXISTS host_port integer NOT NULL DEFAULT 0;
ALTER TABLE gitops_syncs ADD COLUMN IF NOT EXISTS railpack_info text;
`
	v114SQLiteDown = `
ALTER TABLE gitops_syncs DROP COLUMN railpack_info;
ALTER TABLE gitops_syncs DROP COLUMN host_port;
ALTER TABLE gitops_syncs DROP COLUMN app_port;
ALTER TABLE gitops_syncs DROP COLUMN start_command;
ALTER TABLE gitops_syncs DROP COLUMN build_command;
ALTER TABLE gitops_syncs DROP COLUMN source_path;
ALTER TABLE gitops_syncs DROP COLUMN build_strategy;
`
	v114PostgresDown = `
ALTER TABLE gitops_syncs DROP COLUMN IF EXISTS railpack_info;
ALTER TABLE gitops_syncs DROP COLUMN IF EXISTS host_port;
ALTER TABLE gitops_syncs DROP COLUMN IF EXISTS app_port;
ALTER TABLE gitops_syncs DROP COLUMN IF EXISTS start_command;
ALTER TABLE gitops_syncs DROP COLUMN IF EXISTS build_command;
ALTER TABLE gitops_syncs DROP COLUMN IF EXISTS source_path;
ALTER TABLE gitops_syncs DROP COLUMN IF EXISTS build_strategy;
`
)

func init() {
	goose.AddMigrationContext(run(v114SQLiteUp, v114PostgresUp), run(v114SQLiteDown, v114PostgresDown))
}
