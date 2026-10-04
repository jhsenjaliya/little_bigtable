package bttest

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	emptypb "github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// operationsServer is the durable google.longrunning.Operations registry.
// Every admin method that returns an operation registers it here first.
// Local work completes synchronously, so stored operations are always done;
// their names, metadata and results stay queryable across restarts.
type operationsServer struct {
	longrunningpb.UnimplementedOperationsServer
	db  *sql.DB
	seq atomic.Int64
}

func newOperationsServer(db *sql.DB) *operationsServer {
	o := &operationsServer{db: db}
	o.seq.Store(time.Now().UnixNano())
	return o
}

// operationName follows Bigtable's resource-scoped operation naming:
// operations/{resource}/locations/{location}/operations/{id}.
func (o *operationsServer) operationName(resource string) string {
	return fmt.Sprintf("operations/%s/locations/local/operations/%d", resource, o.seq.Add(1))
}

// complete registers a finished operation with typed metadata and response.
func (o *operationsServer) complete(ctx context.Context, resource string, metadata, response proto.Message) (*longrunningpb.Operation, error) {
	op := &longrunningpb.Operation{Name: o.operationName(resource), Done: true}
	if metadata != nil {
		md, err := anypb.New(metadata)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "wrap operation metadata: %v", err)
		}
		op.Metadata = md
	}
	if response == nil {
		response = &emptypb.Empty{}
	}
	resp, err := anypb.New(response)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "wrap operation response: %v", err)
	}
	op.Result = &longrunningpb.Operation_Response{Response: resp}
	if err := o.save(ctx, op); err != nil {
		return nil, err
	}
	return op, nil
}

func (o *operationsServer) save(ctx context.Context, op *longrunningpb.Operation) error {
	if o == nil || o.db == nil {
		return nil
	}
	data, err := proto.Marshal(op)
	if err != nil {
		return status.Errorf(codes.Internal, "encode operation: %v", err)
	}
	_, err = o.db.ExecContext(ctx,
		bind("INSERT INTO operations_t (name, data, create_micros) VALUES (?, ?, ?) ON CONFLICT (name) DO UPDATE SET data = ?"),
		op.Name, data, time.Now().UnixMicro(), data)
	if err != nil {
		return status.Errorf(codes.Internal, "save operation: %v", err)
	}
	return nil
}

func (o *operationsServer) load(ctx context.Context, name string) (*longrunningpb.Operation, error) {
	var data []byte
	err := o.db.QueryRowContext(ctx, bind("SELECT data FROM operations_t WHERE name = ?"), name).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, status.Errorf(codes.NotFound, "operation %q not found", name)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load operation: %v", err)
	}
	op := &longrunningpb.Operation{}
	if err := proto.Unmarshal(data, op); err != nil {
		return nil, status.Errorf(codes.Internal, "decode operation: %v", err)
	}
	return op, nil
}

func (o *operationsServer) GetOperation(ctx context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "operation name is required")
	}
	return o.load(ctx, req.GetName())
}

var operationDoneFilter = regexp.MustCompile(`^\s*done\s*=\s*(true|false)\s*$`)

// ListOperations lists operations whose names fall under req.name, for
// example operations/projects/p. The only supported filter is done=true|false.
func (o *operationsServer) ListOperations(ctx context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	prefix := strings.TrimSuffix(req.GetName(), "/operations")
	if prefix == "" {
		return nil, status.Error(codes.InvalidArgument, "operation collection name is required")
	}
	if !strings.HasPrefix(prefix, "operations/") {
		prefix = "operations/" + prefix
	}
	// google.longrunning: return_partial_success results in UNIMPLEMENTED
	// unless the service documents support for it; Bigtable does not.
	if req.GetReturnPartialSuccess() {
		return nil, status.Error(codes.Unimplemented, "return_partial_success is not supported")
	}
	wantDone := ""
	if f := strings.TrimSpace(req.GetFilter()); f != "" {
		m := operationDoneFilter.FindStringSubmatch(f)
		if m == nil {
			return nil, status.Errorf(codes.InvalidArgument, "unsupported operation filter %q; only done=true|false is supported", f)
		}
		wantDone = m[1]
	}
	rows, err := o.db.QueryContext(ctx, bind("SELECT name, data FROM operations_t WHERE name LIKE ? ORDER BY name"), likePrefix(prefix+"/"))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list operations: %v", err)
	}
	var ops []*longrunningpb.Operation
	for rows.Next() {
		var name string
		var data []byte
		if err := rows.Scan(&name, &data); err != nil {
			rows.Close()
			return nil, status.Errorf(codes.Internal, "list operations: %v", err)
		}
		if !strings.HasPrefix(name, prefix+"/") {
			continue // LIKE treats '_' as a wildcard
		}
		op := &longrunningpb.Operation{}
		if err := proto.Unmarshal(data, op); err != nil {
			rows.Close()
			return nil, status.Errorf(codes.Internal, "decode operation: %v", err)
		}
		if wantDone != "" && fmt.Sprint(op.Done) != wantDone {
			continue
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, status.Errorf(codes.Internal, "list operations: %v", err)
	}
	rows.Close()
	page, next, err := paginate(ops, func(op *longrunningpb.Operation) string { return op.Name }, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &longrunningpb.ListOperationsResponse{Operations: page, NextPageToken: next}, nil
}

// WaitOperation returns immediately because local operations complete
// synchronously.
func (o *operationsServer) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "operation name is required")
	}
	return o.load(ctx, req.GetName())
}

func (o *operationsServer) DeleteOperation(ctx context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	if _, err := o.load(ctx, req.GetName()); err != nil {
		return nil, err
	}
	if _, err := o.db.ExecContext(ctx, bind("DELETE FROM operations_t WHERE name = ?"), req.GetName()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete operation: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// CancelOperation is a successful no-op for completed operations, matching
// the google.longrunning contract that cancellation is best effort.
func (o *operationsServer) CancelOperation(ctx context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	if _, err := o.load(ctx, req.GetName()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// likePrefix turns s into a LIKE prefix pattern. '%' becomes the single
// character wildcard, so the pattern may over-match; callers re-check the
// prefix exactly.
func likePrefix(s string) string {
	return strings.ReplaceAll(s, "%", "_") + "%"
}

// paginate returns one page of items ordered by key. Page tokens encode the
// last returned key, so pages stay stable when earlier items are deleted.
func paginate[T any](items []T, key func(T) string, pageSize int32, pageToken string) ([]T, string, error) {
	if pageSize < 0 {
		return nil, "", status.Error(codes.InvalidArgument, "page_size must not be negative")
	}
	sort.SliceStable(items, func(i, j int) bool { return key(items[i]) < key(items[j]) })
	start := 0
	if pageToken != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(pageToken)
		if err != nil {
			return nil, "", status.Errorf(codes.InvalidArgument, "invalid page_token %q", pageToken)
		}
		after := string(decoded)
		start = sort.Search(len(items), func(i int) bool { return key(items[i]) > after })
	}
	items = items[start:]
	if pageSize == 0 || int(pageSize) >= len(items) {
		return items, "", nil
	}
	page := items[:pageSize]
	return page, base64.RawURLEncoding.EncodeToString([]byte(key(page[len(page)-1]))), nil
}
