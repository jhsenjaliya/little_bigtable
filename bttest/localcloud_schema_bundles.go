package bttest

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxSchemaBundlesPerTable = 10
	maxSchemaBundleBytes     = 4 << 20
)

// SqlSchemaBundles persists schema bundles to schema_bundles_t.
type SqlSchemaBundles struct {
	store protoStore[*btapb.SchemaBundle]
}

func NewSqlSchemaBundles(db *sql.DB) *SqlSchemaBundles {
	return &SqlSchemaBundles{store: protoStore[*btapb.SchemaBundle]{
		db: db, table: "schema_bundles_t", parentCol: "table_name",
		newT: func() *btapb.SchemaBundle { return &btapb.SchemaBundle{} },
	}}
}

func (b *SqlSchemaBundles) deleteByTable(ctx context.Context, tableName string) error {
	return b.store.removePrefix(ctx, nil, tableName+"/schemaBundles/")
}

// schemaBundleMessages returns the fully qualified message names a bundle
// defines, validating its descriptors.
func schemaBundleMessages(sb *btapb.SchemaBundle) (map[string]bool, error) {
	if proto.Size(sb) > maxSchemaBundleBytes {
		return nil, status.Errorf(codes.InvalidArgument, "schema bundle exceeds %d bytes", maxSchemaBundleBytes)
	}
	msgs := map[string]bool{}
	switch t := sb.GetType().(type) {
	case *btapb.SchemaBundle_ProtoSchema:
		raw := t.ProtoSchema.GetProtoDescriptors()
		if len(raw) == 0 {
			return nil, status.Error(codes.InvalidArgument, "proto_schema.proto_descriptors is required")
		}
		set := &descriptorpb.FileDescriptorSet{}
		if err := proto.Unmarshal(raw, set); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "proto_descriptors is not a serialized FileDescriptorSet: %v", err)
		}
		if len(set.GetFile()) == 0 {
			return nil, status.Error(codes.InvalidArgument, "proto_descriptors must contain at least one file")
		}
		if err := validateFileDescriptorSet(set); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "proto_descriptors is not a valid FileDescriptorSet: %v", err)
		}
		for _, f := range set.GetFile() {
			var walk func(prefix string, m *descriptorpb.DescriptorProto)
			walk = func(prefix string, m *descriptorpb.DescriptorProto) {
				name := prefix + "." + m.GetName()
				msgs[strings.TrimPrefix(name, ".")] = true
				for _, nested := range m.GetNestedType() {
					walk(name, nested)
				}
			}
			for _, m := range f.GetMessageType() {
				walk(f.GetPackage(), m)
			}
		}
	case *btapb.SchemaBundle_AvroSchema:
		if len(t.AvroSchema.GetJsonSchemas()) == 0 {
			return nil, status.Error(codes.InvalidArgument, "avro_schema.json_schemas is required")
		}
		for i, js := range t.AvroSchema.GetJsonSchemas() {
			// An Avro schema is a JSON object, array (union) or string (primitive).
			var schema any
			if err := json.Unmarshal([]byte(js), &schema); err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "avro_schema.json_schemas[%d] is not valid JSON: %v", i, err)
			}
			switch schema.(type) {
			case map[string]any, []any, string:
			default:
				return nil, status.Errorf(codes.InvalidArgument, "avro_schema.json_schemas[%d] is not an Avro schema", i)
			}
			v, _ := schema.(map[string]any)
			if name, _ := v["name"].(string); name != "" {
				ns, _ := v["namespace"].(string)
				msgs[strings.TrimPrefix(ns+"."+name, ".")] = true
			}
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "a schema bundle requires proto_schema or avro_schema")
	}
	return msgs, nil
}

// validateFileDescriptorSet checks that every file in set is well formed and
// that all type references resolve. Imports omitted from the set are resolved
// from the well-known protos linked into the emulator.
func validateFileDescriptorSet(set *descriptorpb.FileDescriptorSet) error {
	files := proto.Clone(set).(*descriptorpb.FileDescriptorSet)
	have := make(map[string]bool, len(files.GetFile()))
	for _, f := range files.GetFile() {
		have[f.GetName()] = true
	}
	for i := 0; i < len(files.File); i++ {
		for _, dep := range files.File[i].GetDependency() {
			if have[dep] {
				continue
			}
			if fd, err := protoregistry.GlobalFiles.FindFileByPath(dep); err == nil {
				files.File = append(files.File, protodesc.ToFileDescriptorProto(fd))
				have[dep] = true
			}
		}
	}
	_, err := protodesc.NewFiles(files)
	return err
}

func (s *server) CreateSchemaBundle(ctx context.Context, req *btapb.CreateSchemaBundleRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	if !tableIDPattern.MatchString(req.GetSchemaBundleId()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid schema_bundle_id %q", req.GetSchemaBundleId())
	}
	if _, err := s.localRequireTable(req.GetParent()); err != nil {
		return nil, err
	}
	sb := &btapb.SchemaBundle{}
	if req.SchemaBundle != nil {
		sb = proto.Clone(req.SchemaBundle).(*btapb.SchemaBundle)
	}
	if _, err := schemaBundleMessages(sb); err != nil {
		return nil, err
	}
	name := req.Parent + "/schemaBundles/" + req.SchemaBundleId
	sb.Name = name

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok, err := s.sbBackend.store.get(ctx, nil, name); err != nil {
		return nil, internalErr(err)
	} else if ok {
		return nil, status.Errorf(codes.AlreadyExists, "schema bundle %q already exists", name)
	}
	existing, err := s.sbBackend.store.listPrefix(ctx, nil, req.Parent+"/schemaBundles/")
	if err != nil {
		return nil, internalErr(err)
	}
	if len(existing) >= maxSchemaBundlesPerTable {
		return nil, status.Errorf(codes.ResourceExhausted, "table %q already has the maximum of %d schema bundles", req.Parent, maxSchemaBundlesPerTable)
	}
	sb.Etag = contentEtag(sb)
	if err := s.sbBackend.store.put(ctx, nil, name, req.Parent, sb); err != nil {
		return nil, internalErr(err)
	}
	return s.ops.complete(ctx, name, &btapb.CreateSchemaBundleMetadata{Name: name, StartTime: start, EndTime: timestamppb.Now()}, sb)
}

func (s *server) GetSchemaBundle(ctx context.Context, req *btapb.GetSchemaBundleRequest) (*btapb.SchemaBundle, error) {
	sb, ok, err := s.sbBackend.store.get(ctx, nil, req.GetName())
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "schema bundle %q not found", req.GetName())
	}
	return sb, nil
}

func (s *server) ListSchemaBundles(ctx context.Context, req *btapb.ListSchemaBundlesRequest) (*btapb.ListSchemaBundlesResponse, error) {
	if _, err := s.localRequireTable(req.GetParent()); err != nil {
		return nil, err
	}
	bundles, err := s.sbBackend.store.listPrefix(ctx, nil, req.GetParent()+"/schemaBundles/")
	if err != nil {
		return nil, internalErr(err)
	}
	page, next, err := paginate(bundles, (*btapb.SchemaBundle).GetName, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &btapb.ListSchemaBundlesResponse{SchemaBundles: page, NextPageToken: next}, nil
}

func (s *server) UpdateSchemaBundle(ctx context.Context, req *btapb.UpdateSchemaBundleRequest) (*longrunning.Operation, error) {
	start := timestamppb.Now()
	update := req.GetSchemaBundle()
	if update == nil || update.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "schema_bundle.name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok, err := s.sbBackend.store.get(ctx, nil, update.GetName())
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "schema bundle %q not found", update.GetName())
	}
	if err := checkEtag(update.GetEtag(), cur.GetEtag()); err != nil {
		return nil, err
	}
	for _, path := range req.GetUpdateMask().GetPaths() {
		switch path {
		case "proto_schema", "proto_schema.proto_descriptors", "avro_schema", "avro_schema.json_schemas":
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update_mask path %q", path)
		}
	}
	if (cur.GetProtoSchema() != nil) != (update.GetProtoSchema() != nil) {
		return nil, status.Error(codes.InvalidArgument, "the schema type of a schema bundle cannot change after creation")
	}
	newMsgs, err := schemaBundleMessages(update)
	if err != nil {
		return nil, err
	}
	if !req.GetIgnoreWarnings() {
		oldMsgs, _ := schemaBundleMessages(cur)
		for m := range oldMsgs {
			if !newMsgs[m] {
				return nil, status.Errorf(codes.FailedPrecondition, "update removes message %q and is not backward compatible; set ignore_warnings to force it", m)
			}
		}
	}
	next := proto.Clone(update).(*btapb.SchemaBundle)
	next.Etag = contentEtag(next)
	if err := s.sbBackend.store.put(ctx, nil, next.Name, schemaBundleParentTable(next.Name), next); err != nil {
		return nil, internalErr(err)
	}
	return s.ops.complete(ctx, next.Name, &btapb.UpdateSchemaBundleMetadata{Name: next.Name, StartTime: start, EndTime: timestamppb.Now()}, next)
}

func (s *server) DeleteSchemaBundle(ctx context.Context, req *btapb.DeleteSchemaBundleRequest) (*empty.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok, err := s.sbBackend.store.get(ctx, nil, req.GetName())
	if err != nil {
		return nil, internalErr(err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "schema bundle %q not found", req.GetName())
	}
	if err := checkEtag(req.GetEtag(), cur.GetEtag()); err != nil {
		return nil, err
	}
	if err := s.sbBackend.store.remove(ctx, nil, req.GetName()); err != nil {
		return nil, internalErr(err)
	}
	// The bundle's IAM policy is deleted with it; a re-created bundle starts empty.
	if err := s.iamBackend.deletePrefix(ctx, nil, req.GetName()); err != nil {
		return nil, internalErr(err)
	}
	return &empty.Empty{}, nil
}

func schemaBundleParentTable(name string) string {
	if idx := strings.LastIndex(name, "/schemaBundles/"); idx >= 0 {
		return name[:idx]
	}
	return name
}
