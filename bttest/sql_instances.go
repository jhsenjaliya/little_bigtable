package bttest

import (
	"database/sql"
	"log"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/protobuf/proto"
)

// SqlInstances persists instance metadata to instances_t.
type SqlInstances struct {
	db *sql.DB
}

// NewSqlInstances returns a SqlInstances backed by the given DB.
func NewSqlInstances(db *sql.DB) *SqlInstances {
	return &SqlInstances{
		db: db,
	}
}

// Get loads a single instance's metadata from the DB. Returns nil if not found.
func (db *SqlInstances) Get(parent, instanceId string) *btapb.Instance {
	row := db.db.QueryRow(
		"SELECT metadata FROM instances_t WHERE parent = ? AND instance_id = ?",
		parent, instanceId,
	)
	var data []byte
	if err := row.Scan(&data); err != nil {
		return nil
	}
	inst := &btapb.Instance{}
	if err := proto.Unmarshal(data, inst); err != nil {
		return nil
	}
	return inst
}

// GetAll loads all instance metadata from the DB, used to restore state on startup.
func (db *SqlInstances) GetAll() map[string]*btapb.Instance {
	rows, err := db.db.Query("SELECT parent, instance_id, metadata FROM instances_t")
	if err != nil {
		return nil
	}
	defer rows.Close()

	instances := make(map[string]*btapb.Instance)
	for rows.Next() {
		var parent, instanceId string
		var data []byte
		if err := rows.Scan(&parent, &instanceId, &data); err != nil {
			continue
		}
		inst := &btapb.Instance{}
		if err := proto.Unmarshal(data, inst); err != nil {
			continue
		}
		// Key by fully qualified name: projects/{project}/instances/{instance}
		name := parent + "/instances/" + instanceId
		instances[name] = inst
	}
	return instances
}

// Save upserts an instance's metadata to the DB.
func (db *SqlInstances) Save(parent, instanceId string, inst *btapb.Instance) error {
	data, err := proto.Marshal(inst)
	if err != nil {
		return err
	}
	_, err = db.db.Exec(
		"INSERT OR REPLACE INTO instances_t (parent, instance_id, metadata) VALUES (?, ?, ?)",
		parent, instanceId, data,
	)
	return err
}

// Delete removes an instance's metadata record from the DB.
// Errors are logged but not returned — the in-memory state has already been cleared,
// and a failed delete means the row will be restored on next startup (safe).
func (db *SqlInstances) Delete(parent, instanceId string) {
	_, err := db.db.Exec(
		"DELETE FROM instances_t WHERE parent = ? AND instance_id = ?",
		parent, instanceId,
	)
	if err != nil {
		log.Printf("WARNING: failed to delete instance %q/%q from instances_t: %v", parent, instanceId, err)
	}
}
