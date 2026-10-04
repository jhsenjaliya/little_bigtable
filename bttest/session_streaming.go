package bttest

import (
	"context"
	"io"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	statpb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	sessionTargetTable = iota
	sessionTargetAuthorizedView
	sessionTargetMaterializedView
)

type sessionStream interface {
	Send(*btpb.SessionResponse) error
	Recv() (*btpb.SessionRequest, error)
	Context() context.Context
}

// session is one open session: a resolved target and the permissions granted.
type session struct {
	targetType int
	table      string // table name for table sessions
	view       string // authorized or materialized view name
	appProfile string
	canRead    bool
	canWrite   bool
}

// GetClientConfiguration directs clients to the classic RPCs (session load 0)
// and stops polling. Sessions stay available to clients that open them.
func (s *server) GetClientConfiguration(ctx context.Context, req *btpb.GetClientConfigurationRequest) (*btpb.ClientConfiguration, error) {
	if req.GetInstanceName() == "" {
		return nil, status.Error(codes.InvalidArgument, "instance_name is required")
	}
	if err := s.localRequireInstance(req.GetInstanceName()); err != nil {
		return nil, err
	}
	if _, err := s.resolveAppProfile(req.GetInstanceName(), req.GetAppProfileId()); err != nil {
		return nil, err
	}
	return &btpb.ClientConfiguration{
		SessionConfiguration: &btpb.SessionClientConfiguration{SessionLoad: 0},
		Polling:              &btpb.ClientConfiguration_StopPolling{StopPolling: true},
	}, nil
}

func (s *server) OpenTable(stream btpb.Bigtable_OpenTableServer) error {
	return s.handleSessionStream(stream, sessionTargetTable)
}

func (s *server) OpenAuthorizedView(stream btpb.Bigtable_OpenAuthorizedViewServer) error {
	return s.handleSessionStream(stream, sessionTargetAuthorizedView)
}

func (s *server) OpenMaterializedView(stream btpb.Bigtable_OpenMaterializedViewServer) error {
	return s.handleSessionStream(stream, sessionTargetMaterializedView)
}

func (s *server) handleSessionStream(stream sessionStream, targetType int) error {
	var sess *session
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch p := req.Payload.(type) {
		case *btpb.SessionRequest_OpenSession:
			if sess != nil {
				return status.Error(codes.FailedPrecondition, "the session is already open")
			}
			opened, respPayload, err := s.openSession(targetType, p.OpenSession.GetPayload())
			if err != nil {
				return err
			}
			sess = opened
			if err := stream.Send(&btpb.SessionResponse{Payload: &btpb.SessionResponse_OpenSession{
				OpenSession: &btpb.OpenSessionResponse{
					Payload: respPayload,
					Backend: &btpb.BackendIdentifier{GoogleFrontendId: 1, ApplicationFrontendId: 1, ApplicationFrontendZone: "local"},
				},
			}}); err != nil {
				return err
			}
		case *btpb.SessionRequest_VirtualRpc:
			vrpc := p.VirtualRpc
			if sess == nil {
				return status.Error(codes.FailedPrecondition, "a session must be opened before sending virtual RPCs")
			}
			payload, err := s.sessionVirtualRPC(stream.Context(), sess, vrpc.GetPayload())
			if err != nil {
				st := status.Convert(err)
				if err := stream.Send(&btpb.SessionResponse{Payload: &btpb.SessionResponse_Error{Error: &btpb.ErrorResponse{
					RpcId:  vrpc.GetRpcId(),
					Status: &statpb.Status{Code: int32(st.Code()), Message: st.Message()},
				}}}); err != nil {
					return err
				}
				continue
			}
			if err := stream.Send(&btpb.SessionResponse{Payload: &btpb.SessionResponse_VirtualRpc{
				VirtualRpc: &btpb.VirtualRpcResponse{RpcId: vrpc.GetRpcId(), Payload: payload},
			}}); err != nil {
				return err
			}
		case *btpb.SessionRequest_CloseSession:
			return nil
		default:
			return status.Errorf(codes.InvalidArgument, "unsupported session request %T", p)
		}
	}
}

// openSession resolves and validates the session target before the session
// is acknowledged.
func (s *server) openSession(targetType int, payload []byte) (*session, []byte, error) {
	switch targetType {
	case sessionTargetTable:
		var req btpb.OpenTableRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, nil, status.Errorf(codes.InvalidArgument, "invalid OpenTableRequest: %v", err)
		}
		read, write := sessionPermissions(int32(req.GetPermission()))
		sess := &session{targetType: targetType, table: req.GetTableName(), appProfile: req.GetAppProfileId(), canRead: read, canWrite: write}
		if _, err := s.resolveReadTarget(req.GetTableName(), "", "", req.GetAppProfileId()); err != nil {
			return nil, nil, err
		}
		if write {
			if _, err := s.resolveWriteTarget(req.GetTableName(), "", req.GetAppProfileId(), false); err != nil {
				return nil, nil, err
			}
		}
		resp, _ := proto.Marshal(&btpb.OpenTableResponse{})
		return sess, resp, nil
	case sessionTargetAuthorizedView:
		var req btpb.OpenAuthorizedViewRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, nil, status.Errorf(codes.InvalidArgument, "invalid OpenAuthorizedViewRequest: %v", err)
		}
		read, write := sessionPermissions(int32(req.GetPermission()))
		sess := &session{targetType: targetType, view: req.GetAuthorizedViewName(), appProfile: req.GetAppProfileId(), canRead: read, canWrite: write}
		if _, err := s.resolveReadTarget("", req.GetAuthorizedViewName(), "", req.GetAppProfileId()); err != nil {
			return nil, nil, err
		}
		if write {
			if _, err := s.resolveWriteTarget("", req.GetAuthorizedViewName(), req.GetAppProfileId(), false); err != nil {
				return nil, nil, err
			}
		}
		resp, _ := proto.Marshal(&btpb.OpenAuthorizedViewResponse{})
		return sess, resp, nil
	case sessionTargetMaterializedView:
		var req btpb.OpenMaterializedViewRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, nil, status.Errorf(codes.InvalidArgument, "invalid OpenMaterializedViewRequest: %v", err)
		}
		sess := &session{targetType: targetType, view: req.GetMaterializedViewName(), appProfile: req.GetAppProfileId(), canRead: true}
		if _, err := s.resolveReadTarget("", "", req.GetMaterializedViewName(), req.GetAppProfileId()); err != nil {
			return nil, nil, err
		}
		resp, _ := proto.Marshal(&btpb.OpenMaterializedViewResponse{})
		return sess, resp, nil
	}
	return nil, nil, status.Error(codes.Internal, "unknown session target")
}

// sessionPermissions maps an Open*Request permission; unset grants both.
func sessionPermissions(p int32) (read, write bool) {
	switch p {
	case 1:
		return true, false
	case 2:
		return false, true
	default:
		return true, true
	}
}

func (s *server) sessionVirtualRPC(ctx context.Context, sess *session, payload []byte) ([]byte, error) {
	var req btpb.TableRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid TableRequest: %v", err)
	}
	var resp btpb.TableResponse
	switch {
	case req.GetReadRow() != nil:
		if !sess.canRead {
			return nil, status.Error(codes.PermissionDenied, "the session was opened without read permission")
		}
		row, err := s.sessionReadRow(ctx, sess, req.GetReadRow())
		if err != nil {
			return nil, err
		}
		resp.Payload = &btpb.TableResponse_ReadRow{ReadRow: &btpb.SessionReadRowResponse{Row: row}}
	case req.GetMutateRow() != nil:
		if !sess.canWrite {
			return nil, status.Error(codes.PermissionDenied, "the session was opened without write permission")
		}
		mreq := &btpb.MutateRowRequest{AppProfileId: sess.appProfile, RowKey: req.GetMutateRow().GetKey(), Mutations: req.GetMutateRow().GetMutations()}
		if sess.targetType == sessionTargetAuthorizedView {
			mreq.AuthorizedViewName = sess.view
		} else {
			mreq.TableName = sess.table
		}
		if _, err := s.MutateRow(ctx, mreq); err != nil {
			return nil, err
		}
		resp.Payload = &btpb.TableResponse_MutateRow{MutateRow: &btpb.SessionMutateRowResponse{}}
	default:
		return nil, status.Error(codes.Unimplemented, "unsupported session table request")
	}
	out, err := proto.Marshal(&resp)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode TableResponse: %v", err)
	}
	return out, nil
}

func (s *server) sessionReadRow(ctx context.Context, sess *session, req *btpb.SessionReadRowRequest) (*btpb.Row, error) {
	var tableName, avName, mvName string
	switch sess.targetType {
	case sessionTargetTable:
		tableName = sess.table
	case sessionTargetAuthorizedView:
		avName = sess.view
	case sessionTargetMaterializedView:
		mvName = sess.view
	}
	target, err := s.resolveReadTarget(tableName, avName, mvName, sess.appProfile)
	if err != nil {
		return nil, err
	}
	if len(req.GetKey()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "row key must be non-empty")
	}
	if err := validateFilter(req.GetFilter()); err != nil {
		return nil, err
	}
	if target.mv != nil {
		if err := target.mv.readable(); err != nil {
			return nil, err
		}
	}
	tbl := target.tbl
	tbl.mu.RLock()
	stored, err := tbl.rows.load(ctx, nil, string(req.GetKey()))
	gcRules := tbl.gcRulesNoLock()
	families := make(map[string]bool, len(tbl.families))
	for f := range tbl.families {
		families[f] = true
	}
	tbl.mu.RUnlock()
	if err != nil {
		return nil, storageErr(err)
	}
	if stored == nil {
		return nil, nil
	}
	for f := range stored.families {
		if !families[f] {
			delete(stored.families, f)
		}
	}
	stored.gc(gcRules)
	if target.view != nil {
		stored = target.view.restrict(stored)
	}
	if _, err := filterRow(req.GetFilter(), stored); err != nil {
		return nil, err
	}
	if stored.isEmpty() {
		return nil, nil
	}
	return stored.toProto(), nil
}

func (r *row) toProto() *btpb.Row {
	if r == nil {
		return nil
	}
	pbRow := &btpb.Row{Key: []byte(r.key)}
	for _, f := range r.sortedFamilies() {
		pbFam := &btpb.Family{Name: f.Name}
		for _, colName := range f.ColNames {
			cells := f.Cells[colName]
			if len(cells) == 0 {
				continue
			}
			pbCol := &btpb.Column{Qualifier: []byte(colName)}
			for _, c := range cells {
				pbCol.Cells = append(pbCol.Cells, &btpb.Cell{Value: c.Value, TimestampMicros: c.Ts, Labels: c.Labels})
			}
			pbFam.Columns = append(pbFam.Columns, pbCol)
		}
		if len(pbFam.Columns) > 0 {
			pbRow.Families = append(pbRow.Families, pbFam)
		}
	}
	return pbRow
}
