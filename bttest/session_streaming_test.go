package bttest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func TestStreamingSessionHandlers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared", newDBFile(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if err := CreateTables(ctx, db); err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer("127.0.0.1:0", db)
	if err != nil {
		t.Fatalf("failed to start test server: %v", err)
	}
	defer srv.Close()

	conn, err := grpc.DialContext(ctx, srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("failed to connect to server: %v", err)
	}
	defer conn.Close()

	adminClient := btapb.NewBigtableTableAdminClient(conn)
	dataClient := btpb.NewBigtableClient(conn)

	tableName := "projects/test-p/instances/test-i/tables/session-test-tbl"
	_, err = adminClient.CreateTable(ctx, &btapb.CreateTableRequest{
		Parent:  "projects/test-p/instances/test-i",
		TableId: "session-test-tbl",
		Table: &btapb.Table{
			ColumnFamilies: map[string]*btapb.ColumnFamily{
				"cf": {},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	// 1. Test OpenTable
	openTableStream, err := dataClient.OpenTable(ctx)
	if err != nil {
		t.Fatalf("OpenTable RPC failed: %v", err)
	}

	otReqBytes, _ := proto.Marshal(&btpb.OpenTableRequest{TableName: tableName})
	if err := openTableStream.Send(&btpb.SessionRequest{
		Payload: &btpb.SessionRequest_OpenSession{
			OpenSession: &btpb.OpenSessionRequest{
				ProtocolVersion: 1,
				Payload:         otReqBytes,
			},
		},
	}); err != nil {
		t.Fatalf("send OpenSession failed: %v", err)
	}

	resp, err := openTableStream.Recv()
	if err != nil {
		t.Fatalf("recv OpenSession response failed: %v", err)
	}
	if resp.GetOpenSession() == nil {
		t.Fatalf("expected OpenSession response, got: %v", resp)
	}

	// Virtual RPC: MutateRow
	mutateReqBytes, _ := proto.Marshal(&btpb.TableRequest{
		Payload: &btpb.TableRequest_MutateRow{
			MutateRow: &btpb.SessionMutateRowRequest{
				Key: []byte("row1"),
				Mutations: []*btpb.Mutation{
					{
						Mutation: &btpb.Mutation_SetCell_{
							SetCell: &btpb.Mutation_SetCell{
								FamilyName:      "cf",
								ColumnQualifier: []byte("q"),
								TimestampMicros: 1000,
								Value:           []byte("v1"),
							},
						},
					},
				},
			},
		},
	})
	if err := openTableStream.Send(&btpb.SessionRequest{
		Payload: &btpb.SessionRequest_VirtualRpc{
			VirtualRpc: &btpb.VirtualRpcRequest{
				RpcId:   42,
				Payload: mutateReqBytes,
			},
		},
	}); err != nil {
		t.Fatalf("send VirtualRpc mutate failed: %v", err)
	}

	vMutResp, err := openTableStream.Recv()
	if err != nil {
		t.Fatalf("recv VirtualRpc mutate response failed: %v", err)
	}
	if vMutResp.GetVirtualRpc() == nil || vMutResp.GetVirtualRpc().RpcId != 42 {
		t.Fatalf("expected VirtualRpc with RpcId=42, got: %v", vMutResp)
	}

	// Virtual RPC: ReadRow
	readReqBytes, _ := proto.Marshal(&btpb.TableRequest{
		Payload: &btpb.TableRequest_ReadRow{
			ReadRow: &btpb.SessionReadRowRequest{
				Key: []byte("row1"),
			},
		},
	})
	if err := openTableStream.Send(&btpb.SessionRequest{
		Payload: &btpb.SessionRequest_VirtualRpc{
			VirtualRpc: &btpb.VirtualRpcRequest{
				RpcId:   43,
				Payload: readReqBytes,
			},
		},
	}); err != nil {
		t.Fatalf("send VirtualRpc read failed: %v", err)
	}

	vReadResp, err := openTableStream.Recv()
	if err != nil {
		t.Fatalf("recv VirtualRpc read response failed: %v", err)
	}
	if vReadResp.GetVirtualRpc() == nil || vReadResp.GetVirtualRpc().RpcId != 43 {
		t.Fatalf("expected VirtualRpc with RpcId=43, got: %v", vReadResp)
	}

	var readTableResp btpb.TableResponse
	if err := proto.Unmarshal(vReadResp.GetVirtualRpc().Payload, &readTableResp); err != nil {
		t.Fatalf("unmarshal read TableResponse failed: %v", err)
	}
	readRow := readTableResp.GetReadRow()
	if readRow == nil || readRow.Row == nil || string(readRow.Row.Key) != "row1" {
		t.Fatalf("unexpected read row: %v", readRow)
	}

	// 2. Test OpenAuthorizedView
	avStream, err := dataClient.OpenAuthorizedView(ctx)
	if err != nil {
		t.Fatalf("OpenAuthorizedView RPC failed: %v", err)
	}
	avReqBytes, _ := proto.Marshal(&btpb.OpenAuthorizedViewRequest{
		AuthorizedViewName: tableName + "/authorizedViews/av1",
	})
	if err := avStream.Send(&btpb.SessionRequest{
		Payload: &btpb.SessionRequest_OpenSession{
			OpenSession: &btpb.OpenSessionRequest{
				ProtocolVersion: 1,
				Payload:         avReqBytes,
			},
		},
	}); err != nil {
		t.Fatalf("send OpenAuthorizedView OpenSession failed: %v", err)
	}
	avResp, err := avStream.Recv()
	if err != nil || avResp.GetOpenSession() == nil {
		t.Fatalf("recv OpenAuthorizedView response failed: %v", err)
	}

	// 3. Test OpenMaterializedView
	mvStream, err := dataClient.OpenMaterializedView(ctx)
	if err != nil {
		t.Fatalf("OpenMaterializedView RPC failed: %v", err)
	}
	mvReqBytes, _ := proto.Marshal(&btpb.OpenMaterializedViewRequest{
		MaterializedViewName: "projects/test-p/instances/test-i/materializedViews/mv1",
	})
	if err := mvStream.Send(&btpb.SessionRequest{
		Payload: &btpb.SessionRequest_OpenSession{
			OpenSession: &btpb.OpenSessionRequest{
				ProtocolVersion: 1,
				Payload:         mvReqBytes,
			},
		},
	}); err != nil {
		t.Fatalf("send OpenMaterializedView OpenSession failed: %v", err)
	}
	mvResp, err := mvStream.Recv()
	if err != nil || mvResp.GetOpenSession() == nil {
		t.Fatalf("recv OpenMaterializedView response failed: %v", err)
	}
}
