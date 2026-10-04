package bttest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"fmt"
	"log"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/protobuf/proto"
)

// SqlTables persists table metadata to tables_t.
type SqlTables struct {
	db *sql.DB
}

// NewSqlTables returns a SqlTables backed by the given DB.
func NewSqlTables(db *sql.DB) *SqlTables {
	return &SqlTables{db: db}
}

// tableMetadataMagic prefixes the current tables_t.metadata encoding. Records
// without it are legacy gob-encoded family maps and are migrated on load.
var tableMetadataMagic = []byte("LBT\x02")

// tableRecord is the persisted form of a table. Schema fields use the
// Bigtable protobuf so every supported field round-trips exactly.
type tableRecord struct {
	Table         []byte // proto-encoded btapb.Table without name
	FamilyOrder   map[string]uint64
	Counter       uint64
	InitialSplits []string
	CreateMicros  int64
	DeleteMicros  int64
	TableID       string // logical table ID; differs from the storage ID for tombstones
}

// legacyColumnFamily decodes the pre-migration gob format.
type legacyColumnFamily struct {
	Name   string
	Order  uint64
	GCRule *btapb.GcRule
}

func (t *table) encodeMetadata() ([]byte, error) {
	pb := t.schemaProtoNoLock()
	pb.Name = ""
	pb.EffectiveAutomatedBackupPolicy = nil
	tableBytes, err := proto.Marshal(pb)
	if err != nil {
		return nil, err
	}
	rec := tableRecord{
		Table:         tableBytes,
		FamilyOrder:   make(map[string]uint64, len(t.families)),
		Counter:       t.counter,
		InitialSplits: t.initialSplits,
		TableID:       t.tableId,
	}
	for id, cf := range t.families {
		rec.FamilyOrder[id] = cf.Order
	}
	if !t.createTime.IsZero() {
		rec.CreateMicros = t.createTime.UnixMicro()
	}
	if !t.deleteTime.IsZero() {
		rec.DeleteMicros = t.deleteTime.UnixMicro()
	}
	buf := bytes.NewBuffer(append([]byte(nil), tableMetadataMagic...))
	if err := gob.NewEncoder(buf).Encode(rec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeMetadata fills t from a stored record. It reports whether the record
// used the legacy encoding and therefore needs to be rewritten.
func (t *table) decodeMetadata(data []byte) (legacy bool, err error) {
	if !bytes.HasPrefix(data, tableMetadataMagic) {
		var fams map[string]*legacyColumnFamily
		if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&fams); err != nil {
			return false, fmt.Errorf("decode legacy table metadata: %w", err)
		}
		t.families = make(map[string]*columnFamily, len(fams))
		for id, f := range fams {
			t.families[id] = &columnFamily{Name: t.name() + "/columnFamilies/" + id, Order: f.Order, GCRule: f.GCRule}
			if f.Order >= t.counter {
				t.counter = f.Order + 1
			}
		}
		t.granularity = btapb.Table_MILLIS
		return true, nil
	}

	var rec tableRecord
	if err := gob.NewDecoder(bytes.NewReader(data[len(tableMetadataMagic):])).Decode(&rec); err != nil {
		return false, fmt.Errorf("decode table metadata: %w", err)
	}
	pb := &btapb.Table{}
	if err := proto.Unmarshal(rec.Table, pb); err != nil {
		return false, fmt.Errorf("decode table schema: %w", err)
	}
	if rec.TableID != "" {
		t.tableId = rec.TableID
	}
	t.families = make(map[string]*columnFamily, len(pb.GetColumnFamilies()))
	for id, cf := range pb.GetColumnFamilies() {
		t.families[id] = &columnFamily{
			Name:      t.name() + "/columnFamilies/" + id,
			Order:     rec.FamilyOrder[id],
			GCRule:    cf.GetGcRule(),
			ValueType: cf.GetValueType(),
		}
	}
	t.counter = rec.Counter
	t.granularity = pb.GetGranularity()
	t.deletionProtection = pb.GetDeletionProtection()
	t.changeStream = pb.GetChangeStreamConfig()
	t.rowKeySchema = pb.GetRowKeySchema()
	t.tieredStorage = pb.GetTieredStorageConfig()
	t.restoreInfo = pb.GetRestoreInfo()
	t.backupPolicy = getAutomatedBackupPolicy(pb)
	t.initialSplits = rec.InitialSplits
	if rec.CreateMicros > 0 {
		t.createTime = time.UnixMicro(rec.CreateMicros)
	}
	if rec.DeleteMicros > 0 {
		t.deleteTime = time.UnixMicro(rec.DeleteMicros)
	}
	return false, nil
}

// GetAll loads all table metadata, used to restore state on startup. Legacy
// records are rewritten in the current encoding.
func (db *SqlTables) GetAll(ctx context.Context) ([]*table, error) {
	rows, err := db.db.QueryContext(ctx, "SELECT parent, table_id, metadata FROM tables_t")
	if err != nil {
		return nil, fmt.Errorf("load tables: %w", err)
	}
	type stored struct {
		parent, id string
		metadata   []byte
	}
	var records []stored
	for rows.Next() {
		var rec stored
		if err := rows.Scan(&rec.parent, &rec.id, &rec.metadata); err != nil {
			rows.Close()
			return nil, fmt.Errorf("load tables: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("load tables: %w", err)
	}
	rows.Close()

	var tables []*table
	for _, rec := range records {
		t := &table{parent: rec.parent, tableId: rec.id, storageId: rec.id, rows: NewSqlRows(db.db, rec.parent, rec.id)}
		legacy, err := t.decodeMetadata(rec.metadata)
		if err != nil {
			log.Printf("WARNING: skipping table %s/tables/%s: %v", rec.parent, rec.id, err)
			continue
		}
		if legacy {
			if err := db.Save(ctx, nil, t); err != nil {
				return nil, fmt.Errorf("migrate table %s: %w", t.name(), err)
			}
		}
		tables = append(tables, t)
	}
	return tables, nil
}

// Save upserts a table's metadata. Callers hold t.mu or own t exclusively.
func (db *SqlTables) Save(ctx context.Context, q sqlExecutor, t *table) error {
	metadata, err := t.encodeMetadata()
	if err != nil {
		return fmt.Errorf("encode table %s: %w", t.name(), err)
	}
	if q == nil {
		q = db.db
	}
	_, err = q.ExecContext(ctx,
		bind("INSERT INTO tables_t (parent, table_id, metadata) VALUES (?, ?, ?) ON CONFLICT (parent, table_id) DO UPDATE SET metadata = ?"),
		t.parent, t.storageId, metadata, metadata)
	if err != nil {
		return fmt.Errorf("save table %s: %w", t.name(), err)
	}
	return nil
}

// Delete removes a table's metadata record.
func (db *SqlTables) Delete(ctx context.Context, q sqlExecutor, t *table) error {
	if q == nil {
		q = db.db
	}
	_, err := q.ExecContext(ctx, bind("DELETE FROM tables_t WHERE parent = ? AND table_id = ?"), t.parent, t.storageId)
	if err != nil {
		return fmt.Errorf("delete table %s: %w", t.name(), err)
	}
	return nil
}

// Rename moves a table's metadata and rows to a new storage ID in one
// transaction scope.
func (db *SqlTables) Rename(ctx context.Context, q sqlExecutor, t *table, newStorageID string) error {
	if _, err := q.ExecContext(ctx, bind("UPDATE rows_t SET table_id = ? WHERE parent = ? AND table_id = ?"), newStorageID, t.parent, t.storageId); err != nil {
		return fmt.Errorf("move rows of %s: %w", t.name(), err)
	}
	if _, err := q.ExecContext(ctx, bind("DELETE FROM tables_t WHERE parent = ? AND table_id = ?"), t.parent, t.storageId); err != nil {
		return fmt.Errorf("move table %s: %w", t.name(), err)
	}
	t.storageId = newStorageID
	t.rows = NewSqlRows(db.db, t.parent, newStorageID)
	return db.Save(ctx, q, t)
}
