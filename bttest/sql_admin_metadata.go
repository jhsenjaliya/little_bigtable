package bttest

import (
	"database/sql"
	"log"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/protobuf/proto"
)

type SqlAdminMetadata struct {
	db *sql.DB
}

func NewSqlAdminMetadata(db *sql.DB) *SqlAdminMetadata {
	return &SqlAdminMetadata{db: db}
}

func marshalProto(msg proto.Message) []byte {
	data, err := proto.Marshal(msg)
	if err != nil {
		log.Fatal(err)
	}
	return data
}

func (m *SqlAdminMetadata) SaveInstance(instance *btapb.Instance) {
	_, err := m.db.Exec(
		bind("INSERT INTO instances_t (name, metadata) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET metadata = ?"),
		instance.GetName(), marshalProto(instance), marshalProto(instance),
	)
	if err != nil {
		log.Fatalf("saving instance %q: %v", instance.GetName(), err)
	}
}

func (m *SqlAdminMetadata) DeleteInstance(name string) {
	_, err := m.db.Exec(bind("DELETE FROM instances_t WHERE name = ?"), name)
	if err != nil {
		log.Fatal(err)
	}
	_, err = m.db.Exec(bind("DELETE FROM clusters_t WHERE parent = ?"), name)
	if err != nil {
		log.Fatal(err)
	}
	_, err = m.db.Exec(bind("DELETE FROM app_profiles_t WHERE parent = ?"), name)
	if err != nil {
		log.Fatal(err)
	}
}

func (m *SqlAdminMetadata) GetInstances() []*btapb.Instance {
	rows, err := m.db.Query("SELECT metadata FROM instances_t")
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var result []*btapb.Instance
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			log.Fatal(err)
		}
		inst := &btapb.Instance{}
		if err := proto.Unmarshal(data, inst); err != nil {
			log.Fatal(err)
		}
		result = append(result, inst)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	return result
}

func (m *SqlAdminMetadata) SaveCluster(parent string, cluster *btapb.Cluster) {
	_, err := m.db.Exec(
		bind("INSERT INTO clusters_t (name, parent, metadata) VALUES (?, ?, ?) ON CONFLICT (name) DO UPDATE SET parent = ?, metadata = ?"),
		cluster.GetName(), parent, marshalProto(cluster), parent, marshalProto(cluster),
	)
	if err != nil {
		log.Fatalf("saving cluster %q: %v", cluster.GetName(), err)
	}
}

func (m *SqlAdminMetadata) DeleteCluster(name string) {
	_, err := m.db.Exec(bind("DELETE FROM clusters_t WHERE name = ?"), name)
	if err != nil {
		log.Fatal(err)
	}
}

func (m *SqlAdminMetadata) GetClusters() []*btapb.Cluster {
	rows, err := m.db.Query("SELECT metadata FROM clusters_t")
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var result []*btapb.Cluster
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			log.Fatal(err)
		}
		cluster := &btapb.Cluster{}
		if err := proto.Unmarshal(data, cluster); err != nil {
			log.Fatal(err)
		}
		result = append(result, cluster)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	return result
}

func (m *SqlAdminMetadata) SaveAppProfile(parent string, appProfile *btapb.AppProfile) {
	_, err := m.db.Exec(
		bind("INSERT INTO app_profiles_t (name, parent, metadata) VALUES (?, ?, ?) ON CONFLICT (name) DO UPDATE SET parent = ?, metadata = ?"),
		appProfile.GetName(), parent, marshalProto(appProfile), parent, marshalProto(appProfile),
	)
	if err != nil {
		log.Fatalf("saving app profile %q: %v", appProfile.GetName(), err)
	}
}

func (m *SqlAdminMetadata) DeleteAppProfile(name string) {
	_, err := m.db.Exec(bind("DELETE FROM app_profiles_t WHERE name = ?"), name)
	if err != nil {
		log.Fatal(err)
	}
}

func (m *SqlAdminMetadata) GetAppProfiles() []*btapb.AppProfile {
	rows, err := m.db.Query("SELECT metadata FROM app_profiles_t")
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var result []*btapb.AppProfile
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			log.Fatal(err)
		}
		appProfile := &btapb.AppProfile{}
		if err := proto.Unmarshal(data, appProfile); err != nil {
			log.Fatal(err)
		}
		result = append(result, appProfile)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	return result
}
