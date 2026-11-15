package registry

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Snapshot represents an immutable microVM disk and workspace checkpoint
// from which isolated Copy-on-Write branch instances can be spawned.
type Snapshot struct {
	ID               string            `json:"id"`
	Name             string            `json:"name,omitempty"`
	SourceInstanceID string            `json:"source_instance_id"`
	SourceHandle     string            `json:"source_handle,omitempty"`
	ParentSnapshotID string            `json:"parent_snapshot_id,omitempty"`
	ImageRef         string            `json:"image_ref,omitempty"`
	Command          []string          `json:"command"`
	Env              map[string]string `json:"env,omitempty"`
	SecretKeys       []string          `json:"secret_keys,omitempty"`
	ExposePorts      []int             `json:"expose_ports,omitempty"`
	MemoryMB         int               `json:"memory_mb,omitempty"`
	VCPUs            int               `json:"vcpus,omitempty"`
	DiskMB           int               `json:"disk_mb,omitempty"`
	RootfsPath       string            `json:"rootfs_path"`
	WorkspacePath    string            `json:"workspace_path,omitempty"`
	VMMStateDir      string            `json:"vmm_state_dir,omitempty"`
	LogicalBytes     uint64            `json:"logical_bytes"`
	CoWSavedBytes    uint64            `json:"cow_saved_bytes"`
	CreatedAt        time.Time         `json:"created_at"`
}

// SaveSnapshot inserts or updates a snapshot metadata record.
func (d *DB) SaveSnapshot(s *Snapshot) error {
	cmdJSON, _ := json.Marshal(s.Command)
	envJSON, _ := json.Marshal(s.Env)
	secretKeysJSON, _ := json.Marshal(s.SecretKeys)
	portsJSON, _ := json.Marshal(s.ExposePorts)

	createdAt := s.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	_, err := d.db.Exec(`
		INSERT INTO snapshots (
			id, name, source_instance_id, source_handle, parent_snapshot_id,
			image_ref, command, env, secret_keys, expose_ports,
			memory_mb, vcpus, disk_mb, rootfs_path, workspace_path,
			vmm_state_dir, logical_bytes, cow_saved_bytes, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			rootfs_path = excluded.rootfs_path,
			workspace_path = excluded.workspace_path,
			vmm_state_dir = excluded.vmm_state_dir,
			logical_bytes = excluded.logical_bytes,
			cow_saved_bytes = excluded.cow_saved_bytes
	`,
		s.ID, s.Name, s.SourceInstanceID, s.SourceHandle, s.ParentSnapshotID,
		s.ImageRef, string(cmdJSON), string(envJSON), string(secretKeysJSON), string(portsJSON),
		s.MemoryMB, s.VCPUs, s.DiskMB, s.RootfsPath, s.WorkspacePath,
		s.VMMStateDir, int64(s.LogicalBytes), int64(s.CoWSavedBytes), createdAt.Format(time.RFC3339),
	)
	return err
}

// GetSnapshot retrieves a snapshot by ID. Returns (nil, nil) if not found.
func (d *DB) GetSnapshot(id string) (*Snapshot, error) {
	row := d.db.QueryRow(`
		SELECT id, name, source_instance_id, source_handle, parent_snapshot_id,
		       image_ref, command, env, secret_keys, expose_ports,
		       memory_mb, vcpus, disk_mb, rootfs_path, workspace_path,
		       vmm_state_dir, logical_bytes, cow_saved_bytes, created_at
		FROM snapshots WHERE id = ?
	`, id)
	return scanSnapshot(row)
}

// GetSnapshotByName retrieves the most recent snapshot matching name. Returns (nil, nil) if not found.
func (d *DB) GetSnapshotByName(name string) (*Snapshot, error) {
	if name == "" {
		return nil, nil
	}
	row := d.db.QueryRow(`
		SELECT id, name, source_instance_id, source_handle, parent_snapshot_id,
		       image_ref, command, env, secret_keys, expose_ports,
		       memory_mb, vcpus, disk_mb, rootfs_path, workspace_path,
		       vmm_state_dir, logical_bytes, cow_saved_bytes, created_at
		FROM snapshots WHERE name = ?
		ORDER BY created_at DESC, id DESC LIMIT 1
	`, name)
	return scanSnapshot(row)
}

// ListSnapshots returns all snapshots, optionally filtered by sourceInstanceID.
func (d *DB) ListSnapshots(sourceInstanceID string) ([]*Snapshot, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if sourceInstanceID != "" {
		rows, err = d.db.Query(`
			SELECT id, name, source_instance_id, source_handle, parent_snapshot_id,
			       image_ref, command, env, secret_keys, expose_ports,
			       memory_mb, vcpus, disk_mb, rootfs_path, workspace_path,
			       vmm_state_dir, logical_bytes, cow_saved_bytes, created_at
			FROM snapshots WHERE source_instance_id = ?
			ORDER BY created_at DESC, id DESC
		`, sourceInstanceID)
	} else {
		rows, err = d.db.Query(`
			SELECT id, name, source_instance_id, source_handle, parent_snapshot_id,
			       image_ref, command, env, secret_keys, expose_ports,
			       memory_mb, vcpus, disk_mb, rootfs_path, workspace_path,
			       vmm_state_dir, logical_bytes, cow_saved_bytes, created_at
			FROM snapshots
			ORDER BY created_at DESC, id DESC
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Snapshot
	for rows.Next() {
		s, err := scanSnapshotRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteSnapshot deletes a snapshot metadata record by ID.
func (d *DB) DeleteSnapshot(id string) error {
	res, err := d.db.Exec(`DELETE FROM snapshots WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("snapshot %s not found", id)
	}
	return nil
}

type snapshotScanner interface {
	Scan(dest ...interface{}) error
}

func scanSnapshotCommon(sc snapshotScanner) (*Snapshot, error) {
	var s Snapshot
	var cmdJSON, envJSON, secretsJSON, portsJSON, createdStr string
	var logicalBytes, savedBytes int64

	err := sc.Scan(
		&s.ID, &s.Name, &s.SourceInstanceID, &s.SourceHandle, &s.ParentSnapshotID,
		&s.ImageRef, &cmdJSON, &envJSON, &secretsJSON, &portsJSON,
		&s.MemoryMB, &s.VCPUs, &s.DiskMB, &s.RootfsPath, &s.WorkspacePath,
		&s.VMMStateDir, &logicalBytes, &savedBytes, &createdStr,
	)
	if err != nil {
		return nil, err
	}

	_ = json.Unmarshal([]byte(cmdJSON), &s.Command)
	_ = json.Unmarshal([]byte(envJSON), &s.Env)
	_ = json.Unmarshal([]byte(secretsJSON), &s.SecretKeys)
	_ = json.Unmarshal([]byte(portsJSON), &s.ExposePorts)
	if logicalBytes > 0 {
		s.LogicalBytes = uint64(logicalBytes)
	}
	if savedBytes > 0 {
		s.CoWSavedBytes = uint64(savedBytes)
	}
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return &s, nil
}

func scanSnapshot(row *sql.Row) (*Snapshot, error) {
	s, err := scanSnapshotCommon(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

func scanSnapshotRow(rows *sql.Rows) (*Snapshot, error) {
	return scanSnapshotCommon(rows)
}
