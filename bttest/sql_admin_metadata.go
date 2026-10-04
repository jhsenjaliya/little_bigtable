package bttest

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/protobuf/proto"
)

// SqlAdminMetadata persists instances, clusters and app profiles.
type SqlAdminMetadata struct {
	db *sql.DB
}

func NewSqlAdminMetadata(db *sql.DB) *SqlAdminMetadata {
	return &SqlAdminMetadata{db: db}
}

func (m *SqlAdminMetadata) upsert(table, name, parent string, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	if parent == "" {
		_, err = m.db.Exec(bind("INSERT INTO "+table+" (name, metadata) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET metadata = ?"), name, data, data)
	} else {
		_, err = m.db.Exec(bind("INSERT INTO "+table+" (name, parent, metadata) VALUES (?, ?, ?) ON CONFLICT (name) DO UPDATE SET parent = ?, metadata = ?"), name, parent, data, parent, data)
	}
	if err != nil {
		return fmt.Errorf("save %s: %w", name, err)
	}
	return nil
}

func (m *SqlAdminMetadata) SaveInstance(instance *btapb.Instance) error {
	return m.upsert("instances_t", instance.GetName(), "", instance)
}

func (m *SqlAdminMetadata) SaveCluster(parent string, cluster *btapb.Cluster) error {
	return m.upsert("clusters_t", cluster.GetName(), parent, cluster)
}

func (m *SqlAdminMetadata) SaveAppProfile(parent string, appProfile *btapb.AppProfile) error {
	return m.upsert("app_profiles_t", appProfile.GetName(), parent, appProfile)
}

func (m *SqlAdminMetadata) DeleteCluster(name string) error {
	_, err := m.db.Exec(bind("DELETE FROM clusters_t WHERE name = ?"), name)
	return err
}

func (m *SqlAdminMetadata) DeleteAppProfile(name string) error {
	_, err := m.db.Exec(bind("DELETE FROM app_profiles_t WHERE name = ?"), name)
	return err
}

// deleteInstanceTx removes an instance with its clusters and app profiles.
func (m *SqlAdminMetadata) deleteInstanceTx(ctx context.Context, q sqlExecutor, name string) error {
	for _, stmt := range []string{
		"DELETE FROM instances_t WHERE name = ?",
		"DELETE FROM clusters_t WHERE parent = ?",
		"DELETE FROM app_profiles_t WHERE parent = ?",
	} {
		if _, err := q.ExecContext(ctx, bind(stmt), name); err != nil {
			return err
		}
	}
	return nil
}

func loadAll[T proto.Message](db *sql.DB, table string, newT func() T) []T {
	rows, err := db.Query("SELECT metadata FROM " + table)
	if err != nil {
		log.Printf("load %s: %v", table, err)
		return nil
	}
	defer rows.Close()
	var result []T
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			log.Printf("load %s: %v", table, err)
			return result
		}
		msg := newT()
		if err := proto.Unmarshal(data, msg); err != nil {
			log.Printf("WARNING: skipping undecodable %s record: %v", table, err)
			continue
		}
		result = append(result, msg)
	}
	return result
}

func (m *SqlAdminMetadata) GetInstances() []*btapb.Instance {
	return loadAll(m.db, "instances_t", func() *btapb.Instance { return &btapb.Instance{} })
}

func (m *SqlAdminMetadata) GetClusters() []*btapb.Cluster {
	return loadAll(m.db, "clusters_t", func() *btapb.Cluster { return &btapb.Cluster{} })
}

func (m *SqlAdminMetadata) GetAppProfiles() []*btapb.AppProfile {
	return loadAll(m.db, "app_profiles_t", func() *btapb.AppProfile { return &btapb.AppProfile{} })
}
