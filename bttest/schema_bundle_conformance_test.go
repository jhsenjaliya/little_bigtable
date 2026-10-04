package bttest

// Conformance tests for schema bundles (Create/Get/List/Update/
// DeleteSchemaBundle). The official Go client covers proto bundles; Avro
// bundles, ignore_warnings, etags on delete and pagination use raw gRPC.
//
// References: docs/schema-bundles.txt and protos/admin_v2_table.proto
// (SchemaBundle, ProtoSchema, AvroSchema) in the research scratchpad.

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// cbkMessage builds a message descriptor whose fields are name -> type_name
// (empty type_name means a string field).
func cbkMessage(name string, fields ...[2]string) *descriptorpb.DescriptorProto {
	m := &descriptorpb.DescriptorProto{Name: proto.String(name)}
	for i, f := range fields {
		fd := &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(f[0]),
			Number: proto.Int32(int32(i + 1)),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}
		if f[1] != "" {
			fd.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
			fd.TypeName = proto.String(f[1])
		}
		m.Field = append(m.Field, fd)
	}
	return m
}

// cbkDescriptorSet serializes a one-file FileDescriptorSet in package
// cbk.test.
func cbkDescriptorSet(t *testing.T, deps []string, msgs ...*descriptorpb.DescriptorProto) []byte {
	t.Helper()
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:        proto.String("cbk/test.proto"),
		Package:     proto.String("cbk.test"),
		Syntax:      proto.String("proto3"),
		Dependency:  deps,
		MessageType: msgs,
	}}}
	raw, err := proto.Marshal(set)
	require.NoError(t, err)
	return raw
}

func cbkCustomerOrder(t *testing.T) []byte {
	return cbkDescriptorSet(t, nil,
		cbkMessage("Customer", [2]string{"name", ""}),
		cbkMessage("Order", [2]string{"id", ""}, [2]string{"customer", ".cbk.test.Customer"}))
}

func cbkProtoBundle(raw []byte) *btapb.SchemaBundle {
	return &btapb.SchemaBundle{Type: &btapb.SchemaBundle_ProtoSchema{ProtoSchema: &btapb.ProtoSchema{ProtoDescriptors: raw}}}
}

func cbkAvroBundle(schemas ...string) *btapb.SchemaBundle {
	return &btapb.SchemaBundle{Type: &btapb.SchemaBundle_AvroSchema{AvroSchema: &btapb.AvroSchema{JsonSchemas: schemas}}}
}

const (
	cbkAvroCustomer = `{"type":"record","name":"Customer","namespace":"cbk.avro","fields":[{"name":"name","type":"string"}]}`
	cbkAvroOrder    = `{"type":"record","name":"Order","namespace":"cbk.avro","fields":[{"name":"id","type":"long"}]}`
)

// TestConformanceSchemaBundleProtoCRUD covers proto schema bundles through
// the official client: create/get/list/update/delete, typed LRO metadata,
// AlreadyExists/NotFound, etags, and persistence across a restart.
func TestConformanceSchemaBundleProtoCRUD(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dbFile := newDBFile(t)
	h := cbkStart(t, ctx, dbFile, true)
	require.NoError(t, h.admin.CreateTable(ctx, "events"))
	raw := cbkCustomerOrder(t)
	conf := &bigtable.SchemaBundleConf{TableID: "events", SchemaBundleID: "orders", ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: raw}}

	require.NoError(t, h.admin.CreateSchemaBundle(ctx, conf))
	cbkCode(t, codes.AlreadyExists, h.admin.CreateSchemaBundle(ctx, conf))
	cbkCode(t, codes.NotFound, h.admin.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: "missing", SchemaBundleID: "orders", ProtoSchema: conf.ProtoSchema}))
	_, err := h.tables.CreateSchemaBundle(ctx, &btapb.CreateSchemaBundleRequest{Parent: cbkParent + "/tables/events", SchemaBundleId: "bad id", SchemaBundle: cbkProtoBundle(raw)})
	cbkCode(t, codes.InvalidArgument, err, "invalid schema_bundle_id")

	info, err := h.admin.GetSchemaBundle(ctx, "events", "orders")
	require.NoError(t, err)
	assert.Equal(t, raw, info.SchemaBundle)
	require.NotEmpty(t, info.Etag)
	_, err = h.admin.GetSchemaBundle(ctx, "events", "missing")
	cbkCode(t, codes.NotFound, err)

	names, err := h.admin.SchemaBundles(ctx, "events")
	require.NoError(t, err)
	assert.Equal(t, []string{"orders"}, names)
	_, err = h.admin.SchemaBundles(ctx, "missing")
	cbkCode(t, codes.NotFound, err)

	// A compatible update (adding a message) changes the etag.
	added := cbkDescriptorSet(t, nil,
		cbkMessage("Customer", [2]string{"name", ""}),
		cbkMessage("Order", [2]string{"id", ""}, [2]string{"customer", ".cbk.test.Customer"}),
		cbkMessage("Refund", [2]string{"order", ".cbk.test.Order"}))
	require.NoError(t, h.admin.UpdateSchemaBundle(ctx, bigtable.UpdateSchemaBundleConf{SchemaBundleConf: bigtable.SchemaBundleConf{
		TableID: "events", SchemaBundleID: "orders", ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: added}, Etag: info.Etag,
	}}))
	updated, err := h.admin.GetSchemaBundle(ctx, "events", "orders")
	require.NoError(t, err)
	assert.Equal(t, added, updated.SchemaBundle)
	assert.NotEqual(t, info.Etag, updated.Etag)

	// LRO metadata types.
	op, err := h.tables.CreateSchemaBundle(ctx, &btapb.CreateSchemaBundleRequest{Parent: cbkParent + "/tables/events", SchemaBundleId: "second", SchemaBundle: cbkProtoBundle(raw)})
	require.NoError(t, err)
	createMD := &btapb.CreateSchemaBundleMetadata{}
	require.NoError(t, op.GetMetadata().UnmarshalTo(createMD))
	assert.Equal(t, cbkParent+"/tables/events/schemaBundles/second", createMD.GetName())
	created := &btapb.SchemaBundle{}
	require.NoError(t, op.GetResponse().UnmarshalTo(created))
	assert.NotEmpty(t, created.GetEtag())
	op, err = h.tables.UpdateSchemaBundle(ctx, &btapb.UpdateSchemaBundleRequest{
		SchemaBundle: &btapb.SchemaBundle{Name: created.GetName(), Type: cbkProtoBundle(added).Type},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"proto_schema"}},
	})
	require.NoError(t, err)
	updateMD := &btapb.UpdateSchemaBundleMetadata{}
	require.NoError(t, op.GetMetadata().UnmarshalTo(updateMD))
	assert.Equal(t, created.GetName(), updateMD.GetName())
	_, err = h.tables.UpdateSchemaBundle(ctx, &btapb.UpdateSchemaBundleRequest{
		SchemaBundle: &btapb.SchemaBundle{Name: created.GetName(), Type: cbkProtoBundle(added).Type},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"etag"}},
	})
	cbkCode(t, codes.InvalidArgument, err, "unsupported update_mask path")
	_, err = h.tables.UpdateSchemaBundle(ctx, &btapb.UpdateSchemaBundleRequest{SchemaBundle: &btapb.SchemaBundle{Name: cbkParent + "/tables/events/schemaBundles/missing", Type: cbkProtoBundle(raw).Type}})
	cbkCode(t, codes.NotFound, err)

	// Bundles persist across a restart, etag included.
	h.close()
	h = cbkStart(t, ctx, dbFile, false)
	persisted, err := h.admin.GetSchemaBundle(ctx, "events", "orders")
	require.NoError(t, err)
	assert.Equal(t, updated.Etag, persisted.Etag)
	assert.Equal(t, added, persisted.SchemaBundle)

	require.NoError(t, h.admin.DeleteSchemaBundle(ctx, "events", "orders"))
	_, err = h.admin.GetSchemaBundle(ctx, "events", "orders")
	cbkCode(t, codes.NotFound, err)
	cbkCode(t, codes.NotFound, h.admin.DeleteSchemaBundle(ctx, "events", "orders"))
}

// TestConformanceSchemaBundleAvroCRUD covers Avro schema bundles
// (AvroSchema.json_schemas) through raw gRPC.
func TestConformanceSchemaBundleAvroCRUD(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	require.NoError(t, h.admin.CreateTable(ctx, "events"))
	parent := cbkParent + "/tables/events"
	name := parent + "/schemaBundles/avro"

	_, err := h.tables.CreateSchemaBundle(ctx, &btapb.CreateSchemaBundleRequest{Parent: parent, SchemaBundleId: "avro", SchemaBundle: cbkAvroBundle(cbkAvroCustomer, cbkAvroOrder)})
	require.NoError(t, err)
	got, err := h.tables.GetSchemaBundle(ctx, &btapb.GetSchemaBundleRequest{Name: name})
	require.NoError(t, err)
	assert.Equal(t, []string{cbkAvroCustomer, cbkAvroOrder}, got.GetAvroSchema().GetJsonSchemas())
	assert.Nil(t, got.GetProtoSchema())

	// Primitive and union Avro schemas are valid self-contained schemas.
	_, err = h.tables.CreateSchemaBundle(ctx, &btapb.CreateSchemaBundleRequest{Parent: parent, SchemaBundleId: "primitive", SchemaBundle: cbkAvroBundle(`"string"`, `["null","long"]`)})
	require.NoError(t, err)

	const extra = `{"type":"record","name":"Refund","namespace":"cbk.avro","fields":[]}`
	_, err = h.tables.UpdateSchemaBundle(ctx, &btapb.UpdateSchemaBundleRequest{
		SchemaBundle: &btapb.SchemaBundle{Name: name, Etag: got.GetEtag(), Type: cbkAvroBundle(cbkAvroCustomer, cbkAvroOrder, extra).Type},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"avro_schema"}},
	})
	require.NoError(t, err)
	_, err = h.tables.UpdateSchemaBundle(ctx, &btapb.UpdateSchemaBundleRequest{
		SchemaBundle: &btapb.SchemaBundle{Name: name, Type: cbkAvroBundle(cbkAvroCustomer).Type},
	})
	cbkCode(t, codes.FailedPrecondition, err, "removing Avro records is backward incompatible")

	resp, err := h.tables.ListSchemaBundles(ctx, &btapb.ListSchemaBundlesRequest{Parent: parent})
	require.NoError(t, err)
	require.Len(t, resp.GetSchemaBundles(), 2)
	assert.Equal(t, name, resp.GetSchemaBundles()[0].GetName())
	assert.Len(t, resp.GetSchemaBundles()[0].GetAvroSchema().GetJsonSchemas(), 3)

	_, err = h.tables.DeleteSchemaBundle(ctx, &btapb.DeleteSchemaBundleRequest{Name: name})
	require.NoError(t, err)
	_, err = h.tables.GetSchemaBundle(ctx, &btapb.GetSchemaBundleRequest{Name: name})
	cbkCode(t, codes.NotFound, err)
}

// TestConformanceSchemaBundleInvalidSchemas covers descriptor validation: the
// bundle needs exactly one schema type, proto_descriptors must be a
// non-empty, well-formed FileDescriptorSet whose references resolve (omitted
// well-known imports are tolerated), json_schemas must be valid Avro JSON,
// and the serialized bundle is limited to 4 MB.
func TestConformanceSchemaBundleInvalidSchemas(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	require.NoError(t, h.admin.CreateTable(ctx, "events"))
	parent := cbkParent + "/tables/events"
	create := func(id string, sb *btapb.SchemaBundle) error {
		_, err := h.tables.CreateSchemaBundle(ctx, &btapb.CreateSchemaBundleRequest{Parent: parent, SchemaBundleId: id, SchemaBundle: sb})
		return err
	}
	emptySet, err := proto.Marshal(&descriptorpb.FileDescriptorSet{})
	require.NoError(t, err)
	dupMessages := cbkDescriptorSet(t, nil, cbkMessage("Customer"), cbkMessage("Customer"))
	unresolved := cbkDescriptorSet(t, nil, cbkMessage("Order", [2]string{"customer", ".cbk.test.Missing"}))
	missingImport := cbkDescriptorSet(t, []string{"cbk/missing.proto"}, cbkMessage("Order"))

	for name, sb := range map[string]*btapb.SchemaBundle{
		"no schema type":            {},
		"empty proto_descriptors":   cbkProtoBundle(nil),
		"garbage proto_descriptors": cbkProtoBundle([]byte("not a descriptor set")),
		"no files":                  cbkProtoBundle(emptySet),
		"duplicate message":         cbkProtoBundle(dupMessages),
		"unresolved type reference": cbkProtoBundle(unresolved),
		"missing import":            cbkProtoBundle(missingImport),
		"empty json_schemas":        cbkAvroBundle(),
		"invalid avro json":         cbkAvroBundle(`{"type": "record",`),
		"avro number":               cbkAvroBundle(`42`),
	} {
		cbkCode(t, codes.InvalidArgument, create("invalid", sb), name)
	}

	// Well-known imports left out of the set are resolved locally.
	ts := cbkMessage("Event", [2]string{"at", ".google.protobuf.Timestamp"})
	require.NoError(t, create("wkt", cbkProtoBundle(cbkDescriptorSet(t, []string{"google/protobuf/timestamp.proto"}, ts))))

	// Bundles over 4 MB are rejected (direct call: gRPC caps messages at 4 MB).
	huge := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("cbk/huge.proto"), Package: proto.String("cbk.huge"), Syntax: proto.String("proto3"),
		MessageType:    []*descriptorpb.DescriptorProto{cbkMessage("Big")},
		SourceCodeInfo: &descriptorpb.SourceCodeInfo{Location: []*descriptorpb.SourceCodeInfo_Location{{LeadingComments: proto.String(strings.Repeat("x", 4<<20))}}},
	}}}
	hugeRaw, err := proto.Marshal(huge)
	require.NoError(t, err)
	_, err = h.srv.s.CreateSchemaBundle(ctx, &btapb.CreateSchemaBundleRequest{Parent: parent, SchemaBundleId: "huge", SchemaBundle: cbkProtoBundle(hugeRaw)})
	cbkCode(t, codes.InvalidArgument, err, "bundle over 4 MB")
}

// TestConformanceSchemaBundleLimitPerTable covers "a maximum of 10 schema
// bundles per table" (ResourceExhausted), counted per table.
func TestConformanceSchemaBundleLimitPerTable(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	require.NoError(t, h.admin.CreateTable(ctx, "events"))
	require.NoError(t, h.admin.CreateTable(ctx, "other"))
	raw := cbkCustomerOrder(t)
	create := func(table, id string) error {
		return h.admin.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: table, SchemaBundleID: id, ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: raw}})
	}
	for i := 0; i < 10; i++ {
		require.NoError(t, create("events", "bundle-"+string(rune('a'+i))))
	}
	cbkCode(t, codes.ResourceExhausted, create("events", "bundle-k"))
	require.NoError(t, create("other", "bundle-a"), "the limit is per table")
	require.NoError(t, h.admin.DeleteSchemaBundle(ctx, "events", "bundle-a"))
	require.NoError(t, create("events", "bundle-k"))
}

// TestConformanceSchemaBundleUpdateRules covers UpdateSchemaBundle: the type
// oneof can't change after creation, backward-incompatible changes (removing
// a message) fail with FailedPrecondition unless ignore_warnings is set, and
// a stale etag fails update and delete with ABORTED.
func TestConformanceSchemaBundleUpdateRules(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	require.NoError(t, h.admin.CreateTable(ctx, "events"))
	parent := cbkParent + "/tables/events"
	protoName := parent + "/schemaBundles/proto"
	avroName := parent + "/schemaBundles/avro"
	_, err := h.tables.CreateSchemaBundle(ctx, &btapb.CreateSchemaBundleRequest{Parent: parent, SchemaBundleId: "proto", SchemaBundle: cbkProtoBundle(cbkCustomerOrder(t))})
	require.NoError(t, err)
	_, err = h.tables.CreateSchemaBundle(ctx, &btapb.CreateSchemaBundleRequest{Parent: parent, SchemaBundleId: "avro", SchemaBundle: cbkAvroBundle(cbkAvroCustomer)})
	require.NoError(t, err)
	update := func(name string, sb *btapb.SchemaBundle, etag string, ignoreWarnings bool) error {
		sb = proto.Clone(sb).(*btapb.SchemaBundle)
		sb.Name, sb.Etag = name, etag
		_, err := h.tables.UpdateSchemaBundle(ctx, &btapb.UpdateSchemaBundleRequest{SchemaBundle: sb, IgnoreWarnings: ignoreWarnings})
		return err
	}

	// The schema type oneof is immutable.
	cbkCode(t, codes.InvalidArgument, update(protoName, cbkAvroBundle(cbkAvroCustomer), "", false), "proto -> avro")
	cbkCode(t, codes.InvalidArgument, update(avroName, cbkProtoBundle(cbkCustomerOrder(t)), "", false), "avro -> proto")

	// Removing a message is backward incompatible.
	customerOnly := cbkProtoBundle(cbkDescriptorSet(t, nil, cbkMessage("Customer", [2]string{"name", ""})))
	cbkCode(t, codes.FailedPrecondition, update(protoName, customerOnly, "", false))
	unchanged, err := h.tables.GetSchemaBundle(ctx, &btapb.GetSchemaBundleRequest{Name: protoName})
	require.NoError(t, err)
	assert.Equal(t, cbkCustomerOrder(t), unchanged.GetProtoSchema().GetProtoDescriptors(), "a rejected update changes nothing")
	err = h.admin.UpdateSchemaBundle(ctx, bigtable.UpdateSchemaBundleConf{IgnoreWarnings: true, SchemaBundleConf: bigtable.SchemaBundleConf{
		TableID: "events", SchemaBundleID: "proto", ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: customerOnly.GetProtoSchema().GetProtoDescriptors()},
	}})
	require.NoError(t, err, "ignore_warnings forces an incompatible update")

	// Etags: stale -> ABORTED on update and delete; current -> success.
	cur, err := h.tables.GetSchemaBundle(ctx, &btapb.GetSchemaBundleRequest{Name: protoName})
	require.NoError(t, err)
	cbkCode(t, codes.Aborted, update(protoName, cbkProtoBundle(cbkCustomerOrder(t)), "stale-etag", false))
	require.NoError(t, update(protoName, cbkProtoBundle(cbkCustomerOrder(t)), cur.GetEtag(), false))
	cbkCode(t, codes.Aborted, update(protoName, cbkProtoBundle(cbkCustomerOrder(t)), cur.GetEtag(), false), "etag changed by the update")
	_, err = h.tables.DeleteSchemaBundle(ctx, &btapb.DeleteSchemaBundleRequest{Name: protoName, Etag: cur.GetEtag()})
	cbkCode(t, codes.Aborted, err)
	fresh, err := h.tables.GetSchemaBundle(ctx, &btapb.GetSchemaBundleRequest{Name: protoName})
	require.NoError(t, err)
	_, err = h.tables.DeleteSchemaBundle(ctx, &btapb.DeleteSchemaBundleRequest{Name: protoName, Etag: fresh.GetEtag()})
	require.NoError(t, err)
}

// TestConformanceSchemaBundlePagination covers ListSchemaBundles page_size
// and page_token, with tokens stable across deletion of listed items.
func TestConformanceSchemaBundlePagination(t *testing.T) {
	cbkStrict(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := cbkStart(t, ctx, newDBFile(t), true)
	require.NoError(t, h.admin.CreateTable(ctx, "events"))
	parent := cbkParent + "/tables/events"
	for _, id := range []string{"b-one", "b-two", "b-three", "b-four", "b-five"} {
		_, err := h.tables.CreateSchemaBundle(ctx, &btapb.CreateSchemaBundleRequest{Parent: parent, SchemaBundleId: id, SchemaBundle: cbkAvroBundle(cbkAvroCustomer)})
		require.NoError(t, err)
	}
	page := func(token string) ([]string, string) {
		t.Helper()
		resp, err := h.tables.ListSchemaBundles(ctx, &btapb.ListSchemaBundlesRequest{Parent: parent, PageSize: 2, PageToken: token})
		require.NoError(t, err)
		var ids []string
		for _, sb := range resp.GetSchemaBundles() {
			ids = append(ids, strings.TrimPrefix(sb.GetName(), parent+"/schemaBundles/"))
		}
		return ids, resp.GetNextPageToken()
	}
	first, token := page("")
	assert.Equal(t, []string{"b-five", "b-four"}, first)
	require.NotEmpty(t, token)
	_, err := h.tables.DeleteSchemaBundle(ctx, &btapb.DeleteSchemaBundleRequest{Name: parent + "/schemaBundles/b-five"})
	require.NoError(t, err)
	second, token := page(token)
	assert.Equal(t, []string{"b-one", "b-three"}, second, "deleting a listed bundle doesn't shift later pages")
	third, token := page(token)
	assert.Equal(t, []string{"b-two"}, third)
	assert.Empty(t, token)

	_, err = h.tables.ListSchemaBundles(ctx, &btapb.ListSchemaBundlesRequest{Parent: parent, PageToken: "%%%"})
	cbkCode(t, codes.InvalidArgument, err, "invalid page_token")
}
