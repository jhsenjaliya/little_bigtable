# Java Bigtable Emulator Primary-Source Notes

**Retrieved:** 2026-08-29  
**Purpose:** Evidence for [`../specs/2026-08-29-java-bigtable-emulator-evaluation.md`](../specs/2026-08-29-java-bigtable-emulator-evaluation.md)

## Verified facts

| Fact | Primary source |
| --- | --- |
| Google's Java Bigtable emulator package is a Java test wrapper around the native Bigtable emulator, not an independent Java server implementation. It supplies `BigtableEmulatorRule` and emulator-configured Java clients. | [google-cloud-java Bigtable emulator README](https://github.com/googleapis/google-cloud-java/blob/main/java-bigtable/google-cloud-bigtable-emulator/README.md) |
| Java Data and Table Admin clients expose emulator builders and use an emulator channel without production credentials. | [google-cloud-java Bigtable emulator README](https://github.com/googleapis/google-cloud-java/blob/main/java-bigtable/google-cloud-bigtable-emulator/README.md) |
| Bigtable's public Data API includes unary and server-streaming row, mutation, sample, query, and change-stream RPCs with detailed request/response contracts. | [Data API RPC reference](https://docs.cloud.google.com/bigtable/docs/reference/data/rpc), [detailed v2 API](https://docs.cloud.google.com/bigtable/docs/reference/data/rpc/google.bigtable.v2) |
| Table and Instance Admin are separate gRPC services with broad resource, update, backup, view, IAM-adjacent, and long-running-operation surfaces. | [Admin API RPC reference](https://docs.cloud.google.com/bigtable/docs/reference/admin/rpc), [detailed Admin v2 API](https://docs.cloud.google.com/bigtable/docs/reference/admin/rpc/google.bigtable.admin.v2) |
| Maven Central publishes generated Java gRPC/protobuf artifacts for both Bigtable Data and Admin; release `2.82.0` was current at retrieval. | [Data artifact metadata](https://repo1.maven.org/maven2/com/google/api/grpc/grpc-google-cloud-bigtable-v2/maven-metadata.xml), [Admin artifact metadata](https://repo1.maven.org/maven2/com/google/api/grpc/grpc-google-cloud-bigtable-admin-v2/maven-metadata.xml) |
| Armeria can register generated gRPC `BindableService` implementations with `GrpcServiceBuilder` and host them on an Armeria server. | [Armeria gRPC server guide](https://github.com/line/armeria/blob/main/site-new/src/content/docs/server/grpc.mdx) |
| Armeria provides `useBlockingTaskExecutor(true)` for gRPC services that perform blocking work, avoiding event-loop starvation. | [Armeria threading model](https://github.com/line/armeria/blob/main/site-new/src/content/docs/advanced/threading-model.mdx) |
| gRPC Java exposes readiness, on-ready, cancellation, and manual flow-control APIs through `CallStreamObserver`/`ServerCallStreamObserver`. | [gRPC Java manual flow-control example](https://github.com/grpc/grpc-java/tree/master/examples/src/main/java/io/grpc/examples/manualflowcontrol), [CallStreamObserver source](https://github.com/grpc/grpc-java/blob/master/stub/src/main/java/io/grpc/stub/CallStreamObserver.java) |
| Google's official emulator intentionally differs from production and documents unsupported Admin, IAM, and managed-service behavior. | [Official Bigtable emulator contract](https://docs.cloud.google.com/bigtable/docs/emulator) |

## Repository-verified facts

- LocalCloud already depends on Bigtable Data/Admin Java gRPC artifacts at `2.81.0`.
- LocalCloud already registers many generated Java gRPC `ImplBase` services through Armeria.
- LocalCloud currently compiles the Go emulator into the same Docker image, starts it under Supervisor, connects it to PostgreSQL, and exposes it on the configured Bigtable port.
- The current Go persistence format encodes row and table blobs with Go `encoding/gob`, so a Java engine cannot directly adopt existing persisted data as a stable format.
- A previous LocalCloud Java `BigtableStore` represented metadata and simple JSON cell data; commit `7571b1e` replaced that path with delegation to the Go emulator.

## Architectural inferences

The following are conclusions from the verified facts, not claims made by the external sources:

1. A native Java implementation is technically viable because protocol stubs, Armeria hosting, JDBC, and gRPC flow-control APIs exist.
2. The difficult work is semantic and storage compatibility, not transport scaffolding.
3. Google's own Java wrapper/native-engine split supports keeping a stable process boundary when the native emulator already exists.
4. A Java cutover needs a language-neutral schema and one-time migration because Go gob is not a cross-language persistence contract.
5. Blocking PostgreSQL scans and server-streaming responses require explicit executor and backpressure design; copying synchronous facade patterns would be unsafe for large reads.
