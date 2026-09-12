package bttest

import (
	"context"
	"io"
	"strings"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
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

// OpenTable implements the Bigtable.OpenTable streaming session RPC.
func (s *server) OpenTable(stream btpb.Bigtable_OpenTableServer) error {
	return s.handleSessionStream(stream, sessionTargetTable)
}

// OpenAuthorizedView implements the Bigtable.OpenAuthorizedView streaming session RPC.
func (s *server) OpenAuthorizedView(stream btpb.Bigtable_OpenAuthorizedViewServer) error {
	return s.handleSessionStream(stream, sessionTargetAuthorizedView)
}

// OpenMaterializedView implements the Bigtable.OpenMaterializedView streaming session RPC.
func (s *server) OpenMaterializedView(stream btpb.Bigtable_OpenMaterializedViewServer) error {
	return s.handleSessionStream(stream, sessionTargetMaterializedView)
}

func (s *server) handleSessionStream(stream sessionStream, targetType int) error {
	var targetResource string

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
			openReq := p.OpenSession
			if openReq != nil && len(openReq.Payload) > 0 {
				switch targetType {
				case sessionTargetTable:
					var otReq btpb.OpenTableRequest
					if err := proto.Unmarshal(openReq.Payload, &otReq); err == nil {
						targetResource = otReq.TableName
					}
				case sessionTargetAuthorizedView:
					var avReq btpb.OpenAuthorizedViewRequest
					if err := proto.Unmarshal(openReq.Payload, &avReq); err == nil {
						targetResource = avReq.AuthorizedViewName
					}
				case sessionTargetMaterializedView:
					var mvReq btpb.OpenMaterializedViewRequest
					if err := proto.Unmarshal(openReq.Payload, &mvReq); err == nil {
						targetResource = mvReq.MaterializedViewName
					}
				}
			}

			var openRespBytes []byte
			switch targetType {
			case sessionTargetTable:
				openRespBytes, _ = proto.Marshal(&btpb.OpenTableResponse{})
			case sessionTargetAuthorizedView:
				openRespBytes, _ = proto.Marshal(&btpb.OpenAuthorizedViewResponse{})
			case sessionTargetMaterializedView:
				openRespBytes, _ = proto.Marshal(&btpb.OpenMaterializedViewResponse{})
			}

			resp := &btpb.SessionResponse{
				Payload: &btpb.SessionResponse_OpenSession{
					OpenSession: &btpb.OpenSessionResponse{
						Payload: openRespBytes,
						Backend: &btpb.BackendIdentifier{
							GoogleFrontendId:        1,
							ApplicationFrontendId:   1,
							ApplicationFrontendZone: "us-central1-b",
						},
					},
				},
			}
			if err := stream.Send(resp); err != nil {
				return err
			}

		case *btpb.SessionRequest_VirtualRpc:
			vrpc := p.VirtualRpc
			if vrpc == nil {
				continue
			}

			var tableReq btpb.TableRequest
			if err := proto.Unmarshal(vrpc.Payload, &tableReq); err != nil {
				return status.Errorf(codes.InvalidArgument, "invalid TableRequest in VirtualRpc: %v", err)
			}

			var tableResp btpb.TableResponse
			if readRow := tableReq.GetReadRow(); readRow != nil {
				rowProto, err := s.readRowForSession(stream.Context(), targetResource, targetType, readRow.Key, readRow.Filter)
				if err != nil {
					return err
				}
				tableResp.Payload = &btpb.TableResponse_ReadRow{
					ReadRow: &btpb.SessionReadRowResponse{
						Row: rowProto,
					},
				}
			} else if mutateRow := tableReq.GetMutateRow(); mutateRow != nil {
				actualTable := targetResource
				if targetType == sessionTargetAuthorizedView {
					actualTable = extractTableFromAuthorizedView(targetResource)
				}
				_, err := s.MutateRow(stream.Context(), &btpb.MutateRowRequest{
					TableName: actualTable,
					RowKey:    mutateRow.Key,
					Mutations: mutateRow.Mutations,
				})
				if err != nil {
					return err
				}
				tableResp.Payload = &btpb.TableResponse_MutateRow{
					MutateRow: &btpb.SessionMutateRowResponse{},
				}
			}

			payloadBytes, err := proto.Marshal(&tableResp)
			if err != nil {
				return status.Errorf(codes.Internal, "failed to marshal TableResponse: %v", err)
			}

			resp := &btpb.SessionResponse{
				Payload: &btpb.SessionResponse_VirtualRpc{
					VirtualRpc: &btpb.VirtualRpcResponse{
						RpcId:   vrpc.RpcId,
						Payload: payloadBytes,
					},
				},
			}
			if err := stream.Send(resp); err != nil {
				return err
			}

		case *btpb.SessionRequest_CloseSession:
			return nil
		}
	}
}

func (s *server) readRowForSession(ctx context.Context, target string, targetType int, key []byte, filter *btpb.RowFilter) (*btpb.Row, error) {
	actualTable := target
	if targetType == sessionTargetAuthorizedView {
		actualTable = extractTableFromAuthorizedView(target)
	}

	s.mu.Lock()
	tbl, ok := s.tables[actualTable]
	s.mu.Unlock()
	if !ok {
		return nil, nil
	}

	tbl.mu.RLock()
	defer tbl.mu.RUnlock()
	item := tbl.rows.Get(btreeKey(string(key)))
	if item == nil {
		return nil, nil
	}
	r := item.(*row).copy()
	if filter != nil {
		include, err := filterRow(filter, r)
		if err != nil {
			return nil, err
		}
		if !include {
			return nil, nil
		}
	}
	return r.toProto(), nil
}

func extractTableFromAuthorizedView(avName string) string {
	idx := strings.Index(avName, "/authorizedViews/")
	if idx != -1 {
		return avName[:idx]
	}
	return avName
}

func (r *row) toProto() *btpb.Row {
	if r == nil {
		return nil
	}
	pbRow := &btpb.Row{
		Key: []byte(r.key),
	}
	for _, f := range r.sortedFamilies() {
		pbFam := &btpb.Family{
			Name: f.Name,
		}
		for _, colName := range f.ColNames {
			cells := f.Cells[colName]
			if len(cells) == 0 {
				continue
			}
			pbCol := &btpb.Column{
				Qualifier: []byte(colName),
			}
			for _, c := range cells {
				pbCol.Cells = append(pbCol.Cells, &btpb.Cell{
					Value:           c.Value,
					TimestampMicros: c.Ts,
					Labels:          c.Labels,
				})
			}
			pbFam.Columns = append(pbFam.Columns, pbCol)
		}
		if len(pbFam.Columns) > 0 {
			pbRow.Families = append(pbRow.Families, pbFam)
		}
	}
	return pbRow
}
