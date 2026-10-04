package bttest

// Conformance tests for the session API (google/bigtable/v2/session.proto):
// OpenTable/OpenAuthorizedView validate their target when the session opens,
// per-RPC failures come back as SessionResponse.error while the stream stays
// open, session permissions restrict virtual RPCs, authorized-view sessions
// apply the view's restrictions, and GetClientConfiguration steers clients to
// the classic RPCs.

import (
	"fmt"
	"io"
	"sort"
	"testing"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type ctgSessionStream interface {
	Send(*btpb.SessionRequest) error
	Recv() (*btpb.SessionResponse, error)
	CloseSend() error
}

type ctgSession struct {
	t      *testing.T
	stream ctgSessionStream
	nextID int64
}

// ctgOpen sends OpenSession with the marshalled open request and returns the
// session, or the stream error that rejected it.
func ctgOpen(t *testing.T, stream ctgSessionStream, open proto.Message) (*ctgSession, error) {
	t.Helper()
	payload, err := proto.Marshal(open)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&btpb.SessionRequest{Payload: &btpb.SessionRequest_OpenSession{
		OpenSession: &btpb.OpenSessionRequest{ProtocolVersion: 1, Payload: payload},
	}}))
	resp, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	require.NotNil(t, resp.GetOpenSession(), "want OpenSession response, got %v", resp)
	return &ctgSession{t: t, stream: stream, nextID: 1}, nil
}

// call sends one virtual RPC and returns either its TableResponse or the
// ErrorResponse that answered it.
func (s *ctgSession) call(req *btpb.TableRequest) (*btpb.TableResponse, *btpb.ErrorResponse) {
	s.t.Helper()
	id := s.nextID
	s.nextID++
	payload, err := proto.Marshal(req)
	require.NoError(s.t, err)
	require.NoError(s.t, s.stream.Send(&btpb.SessionRequest{Payload: &btpb.SessionRequest_VirtualRpc{
		VirtualRpc: &btpb.VirtualRpcRequest{RpcId: id, Payload: payload},
	}}))
	resp, err := s.stream.Recv()
	require.NoError(s.t, err)
	if e := resp.GetError(); e != nil {
		require.Equal(s.t, id, e.GetRpcId())
		return nil, e
	}
	v := resp.GetVirtualRpc()
	require.NotNil(s.t, v, "want VirtualRpc response, got %v", resp)
	require.Equal(s.t, id, v.GetRpcId())
	var out btpb.TableResponse
	require.NoError(s.t, proto.Unmarshal(v.GetPayload(), &out))
	return &out, nil
}

func (s *ctgSession) readRow(key string) (*btpb.Row, *btpb.ErrorResponse) {
	s.t.Helper()
	resp, e := s.call(&btpb.TableRequest{Payload: &btpb.TableRequest_ReadRow{ReadRow: &btpb.SessionReadRowRequest{Key: []byte(key)}}})
	if e != nil {
		return nil, e
	}
	return resp.GetReadRow().GetRow(), nil
}

func (s *ctgSession) mutateRow(key string, muts ...*btpb.Mutation) *btpb.ErrorResponse {
	s.t.Helper()
	resp, e := s.call(&btpb.TableRequest{Payload: &btpb.TableRequest_MutateRow{MutateRow: &btpb.SessionMutateRowRequest{Key: []byte(key), Mutations: muts}}})
	if e == nil {
		require.NotNil(s.t, resp.GetMutateRow())
	}
	return e
}

func ctgSetCellIn(fam, col, value string) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
		FamilyName: fam, ColumnQualifier: []byte(col), TimestampMicros: 3000, Value: []byte(value),
	}}}
}

func ctgRowCells(r *btpb.Row) []string {
	var out []string
	for _, f := range r.GetFamilies() {
		for _, c := range f.GetColumns() {
			for _, cell := range c.GetCells() {
				out = append(out, fmt.Sprintf("%s:%s=%s", f.GetName(), c.GetQualifier(), cell.GetValue()))
			}
		}
	}
	sort.Strings(out)
	return out
}

func ctgErrCode(e *btpb.ErrorResponse) codes.Code {
	if e == nil {
		return codes.OK
	}
	return codes.Code(e.GetStatus().GetCode())
}

func TestConformanceSessionOpenValidatesTarget(t *testing.T) {
	e := ctgNewAVEnv(t)
	ctx := e.ctx
	data := btpb.NewBigtableClient(ctgDial(t, ctx))
	_, err := e.instanceAdmin.CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{
		Parent: fmt.Sprintf("projects/%s/instances/%s", e.projectID, e.instanceID), AppProfileId: "boost",
		AppProfile: &btapb.AppProfile{
			RoutingPolicy: &btapb.AppProfile_SingleClusterRouting_{SingleClusterRouting: &btapb.AppProfile_SingleClusterRouting{ClusterId: "test-cluster"}},
			Isolation:     &btapb.AppProfile_DataBoostIsolationReadOnly_{DataBoostIsolationReadOnly: &btapb.AppProfile_DataBoostIsolationReadOnly{}},
		},
	})
	require.NoError(t, err)

	openTable := func(req *btpb.OpenTableRequest) (*ctgSession, error) {
		stream, err := data.OpenTable(ctx)
		require.NoError(t, err)
		return ctgOpen(t, stream, req)
	}
	openView := func(req *btpb.OpenAuthorizedViewRequest) (*ctgSession, error) {
		stream, err := data.OpenAuthorizedView(ctx)
		require.NoError(t, err)
		return ctgOpen(t, stream, req)
	}

	for name, tc := range map[string]struct {
		req  *btpb.OpenTableRequest
		want codes.Code
	}{
		"missing table":         {&btpb.OpenTableRequest{TableName: e.tableName + "-missing"}, codes.NotFound},
		"no table name":         {&btpb.OpenTableRequest{}, codes.InvalidArgument},
		"unknown app profile":   {&btpb.OpenTableRequest{TableName: e.tableName, AppProfileId: "no-such-profile"}, codes.NotFound},
		"data boost read+write": {&btpb.OpenTableRequest{TableName: e.tableName, AppProfileId: "boost"}, codes.FailedPrecondition},
		"data boost write":      {&btpb.OpenTableRequest{TableName: e.tableName, AppProfileId: "boost", Permission: btpb.OpenTableRequest_PERMISSION_WRITE}, codes.FailedPrecondition},
	} {
		_, err := openTable(tc.req)
		require.Equal(t, tc.want, status.Code(err), "%s: %v", name, err)
	}
	_, err = openView(&btpb.OpenAuthorizedViewRequest{AuthorizedViewName: e.viewName("missing")})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = openView(&btpb.OpenAuthorizedViewRequest{AuthorizedViewName: e.tableName + "-missing/authorizedViews/restricted"})
	require.Equal(t, codes.NotFound, status.Code(err))

	// A read-only session through a Data Boost profile is allowed.
	sess, err := openTable(&btpb.OpenTableRequest{TableName: e.tableName, AppProfileId: "boost", Permission: btpb.OpenTableRequest_PERMISSION_READ})
	require.NoError(t, err)
	row, rpcErr := sess.readRow("user#2")
	require.Nil(t, rpcErr)
	require.Equal(t, []string{"cf1:a=6"}, ctgRowCells(row))

	// Opening twice on one stream is a protocol error.
	payload, _ := proto.Marshal(&btpb.OpenTableRequest{TableName: e.tableName})
	require.NoError(t, sess.stream.Send(&btpb.SessionRequest{Payload: &btpb.SessionRequest_OpenSession{OpenSession: &btpb.OpenSessionRequest{Payload: payload}}}))
	_, err = sess.stream.Recv()
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	// A virtual RPC before OpenSession is a protocol error.
	stream, err := data.OpenTable(ctx)
	require.NoError(t, err)
	req, _ := proto.Marshal(&btpb.TableRequest{Payload: &btpb.TableRequest_ReadRow{ReadRow: &btpb.SessionReadRowRequest{Key: []byte("user#1")}}})
	require.NoError(t, stream.Send(&btpb.SessionRequest{Payload: &btpb.SessionRequest_VirtualRpc{VirtualRpc: &btpb.VirtualRpcRequest{RpcId: 1, Payload: req}}}))
	_, err = stream.Recv()
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestConformanceSessionPermissionsAndErrors(t *testing.T) {
	e := ctgNewAVEnv(t)
	ctx := e.ctx
	data := btpb.NewBigtableClient(ctgDial(t, ctx))
	open := func(p btpb.OpenTableRequest_Permission) *ctgSession {
		stream, err := data.OpenTable(ctx)
		require.NoError(t, err)
		sess, err := ctgOpen(t, stream, &btpb.OpenTableRequest{TableName: e.tableName, Permission: p})
		require.NoError(t, err)
		return sess
	}

	// READ: MutateRow fails with an ErrorResponse; the stream stays usable.
	ro := open(btpb.OpenTableRequest_PERMISSION_READ)
	require.Equal(t, codes.PermissionDenied, ctgErrCode(ro.mutateRow("user#2", ctgSetCellIn("cf1", "a", "ro"))))
	row, rpcErr := ro.readRow("user#2")
	require.Nil(t, rpcErr)
	require.Equal(t, []string{"cf1:a=6"}, ctgRowCells(row))
	require.Equal(t, codes.PermissionDenied, ctgErrCode(ro.mutateRow("user#2", ctgSetCellIn("cf1", "a", "ro"))))
	row, rpcErr = ro.readRow("user#2")
	require.Nil(t, rpcErr)
	require.Equal(t, []string{"cf1:a=6"}, ctgRowCells(row), "a rejected session write must not apply")

	// WRITE: ReadRow fails, MutateRow works.
	wo := open(btpb.OpenTableRequest_PERMISSION_WRITE)
	_, rpcErr = wo.readRow("user#2")
	require.Equal(t, codes.PermissionDenied, ctgErrCode(rpcErr))
	require.Nil(t, wo.mutateRow("user#2", ctgSetCellIn("cf1", "a", "wo")))

	// READ_WRITE: per-RPC validation errors come back as ErrorResponse with
	// the RPC's status code, and later RPCs still succeed.
	rw := open(btpb.OpenTableRequest_PERMISSION_READ_WRITE)
	require.Equal(t, codes.NotFound, ctgErrCode(rw.mutateRow("user#2", ctgSetCellIn("nope", "a", "x"))))
	require.Equal(t, codes.InvalidArgument, ctgErrCode(rw.mutateRow("user#2")))
	_, rpcErr = rw.readRow("")
	require.Equal(t, codes.InvalidArgument, ctgErrCode(rpcErr))
	row, rpcErr = rw.readRow("user#2")
	require.Nil(t, rpcErr)
	require.Equal(t, []string{"cf1:a=6", "cf1:a=wo"}, ctgRowCells(row))
	row, rpcErr = rw.readRow("no-such-row")
	require.Nil(t, rpcErr)
	require.Nil(t, row)

	// CloseSession ends the stream cleanly.
	require.NoError(t, rw.stream.Send(&btpb.SessionRequest{Payload: &btpb.SessionRequest_CloseSession{CloseSession: &btpb.CloseSessionRequest{}}}))
	_, err := rw.stream.Recv()
	require.Equal(t, io.EOF, err)
}

func TestConformanceSessionAuthorizedViewRestrictions(t *testing.T) {
	e := ctgNewAVEnv(t)
	ctx := e.ctx
	data := btpb.NewBigtableClient(ctgDial(t, ctx))
	stream, err := data.OpenAuthorizedView(ctx)
	require.NoError(t, err)
	sess, err := ctgOpen(t, stream, &btpb.OpenAuthorizedViewRequest{AuthorizedViewName: e.viewName("restricted")})
	require.NoError(t, err)

	row, rpcErr := sess.readRow("user#1")
	require.Nil(t, rpcErr)
	require.Equal(t, []string{"cf1:a=1", "cf1:pfx-x=3", "cf2:c=4"}, ctgRowCells(row))
	for _, key := range []string{"other#1", "user#9"} {
		row, rpcErr = sess.readRow(key)
		require.Nil(t, rpcErr)
		require.Nil(t, row, key)
	}

	deleteRow := &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromRow_{DeleteFromRow: &btpb.Mutation_DeleteFromRow{}}}
	require.Equal(t, codes.PermissionDenied, ctgErrCode(sess.mutateRow("user#1", ctgSetCellIn("cf1", "b", "x"))))
	require.Equal(t, codes.PermissionDenied, ctgErrCode(sess.mutateRow("other#1", ctgSetCellIn("cf1", "a", "x"))))
	require.Equal(t, codes.PermissionDenied, ctgErrCode(sess.mutateRow("user#1", deleteRow)))
	require.Nil(t, sess.mutateRow("user#3", ctgSetCellIn("cf1", "a", "in-view")))
	row, rpcErr = sess.readRow("user#3")
	require.Nil(t, rpcErr)
	require.Equal(t, []string{"cf1:a=in-view"}, ctgRowCells(row))

	// The table itself only changed for the in-view write.
	require.Equal(t, []string{
		"other#1 cf1:a=7",
		"user#1 cf1:a=1", "user#1 cf1:b=2", "user#1 cf1:pfx-x=3", "user#1 cf2:c=4", "user#1 cf3:z=5",
		"user#2 cf1:a=6",
		"user#3 cf1:a=in-view",
		"user#9 cf3:z=9",
	}, ctgDump(t, ctx, e.client.OpenTable(e.table), bigtable.InfiniteRange("")))

	// A read-only authorized-view session rejects writes.
	stream, err = data.OpenAuthorizedView(ctx)
	require.NoError(t, err)
	ro, err := ctgOpen(t, stream, &btpb.OpenAuthorizedViewRequest{AuthorizedViewName: e.viewName("restricted"), Permission: btpb.OpenAuthorizedViewRequest_PERMISSION_READ})
	require.NoError(t, err)
	require.Equal(t, codes.PermissionDenied, ctgErrCode(ro.mutateRow("user#3", ctgSetCellIn("cf1", "a", "x"))))
	row, rpcErr = ro.readRow("user#3")
	require.Nil(t, rpcErr)
	require.Equal(t, []string{"cf1:a=in-view"}, ctgRowCells(row))
}

func TestConformanceSessionGetClientConfiguration(t *testing.T) {
	env := setupTestEnv(t)
	defer env.cancel()
	data := btpb.NewBigtableClient(ctgDial(t, env.ctx))
	instance := fmt.Sprintf("projects/%s/instances/%s", env.projectID, env.instanceID)

	for _, profile := range []string{"", "default"} {
		cfg, err := data.GetClientConfiguration(env.ctx, &btpb.GetClientConfigurationRequest{InstanceName: instance, AppProfileId: profile})
		require.NoError(t, err)
		// Session load 0 sends every request over the classic RPCs, and the
		// client is told to stop polling for configuration.
		require.NotNil(t, cfg.GetSessionConfiguration())
		require.Zero(t, cfg.GetSessionConfiguration().GetSessionLoad())
		require.True(t, cfg.GetStopPolling())
	}
	_, err := data.GetClientConfiguration(env.ctx, &btpb.GetClientConfigurationRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = data.GetClientConfiguration(env.ctx, &btpb.GetClientConfigurationRequest{InstanceName: "projects/test-project/instances/missing-instance"})
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = data.GetClientConfiguration(env.ctx, &btpb.GetClientConfigurationRequest{InstanceName: instance, AppProfileId: "no-such-profile"})
	require.Equal(t, codes.NotFound, status.Code(err))
}
