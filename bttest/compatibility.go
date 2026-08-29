package bttest

// CompatibilitySupportLevel is the intended local contract for a protocol
// capability. The contract is achieved only when its verification state is
// CompatibilityTestVerified. The ledger is descriptive only; request handlers
// must still validate and implement their behavior independently.
type CompatibilitySupportLevel string

const (
	CompatibilitySupported             CompatibilitySupportLevel = "supported"
	CompatibilityLocallySimplified     CompatibilitySupportLevel = "locally_simplified"
	CompatibilityExplicitlyUnsupported CompatibilitySupportLevel = "explicitly_unsupported"
	CompatibilityNotApplicable         CompatibilitySupportLevel = "not_applicable"
)

// CompatibilityVerification records whether observed runtime behavior matches
// the declared support contract. Phase 0 deliberately keeps known gaps visible
// until the owning conformance phase changes the handlers.
type CompatibilityVerification string

const (
	CompatibilityTestVerified       CompatibilityVerification = "test_verified"
	CompatibilityKnownNonconformant CompatibilityVerification = "known_nonconformant"
	CompatibilityDeclaredUnverified CompatibilityVerification = "declared_unverified"
)

// CompatibilityCapability describes one stable RPC or request-field contract.
type CompatibilityCapability struct {
	ID            string
	Support       CompatibilitySupportLevel
	Verification  CompatibilityVerification
	LocalContract string
	Observed      string
	OwningTest    string
}

// CompatibilityLedger returns a copy of the checked-in capability ledger.
// Callers may use it for documentation and adapter validation, but it must not
// be used to synthesize successful RPC behavior.
func CompatibilityLedger() []CompatibilityCapability {
	ledger := make([]CompatibilityCapability, len(compatibilityLedger))
	copy(ledger, compatibilityLedger)
	return ledger
}

func rpcCapabilityID(service, method string) string {
	return "rpc." + service + "." + method
}

func fieldCapabilityID(message, field string) string {
	return "field." + message + "." + field
}

func buildCompatibilityLedger() []CompatibilityCapability {
	var ledger []CompatibilityCapability

	addRPCs := func(service string, support CompatibilitySupportLevel, verification CompatibilityVerification, contract, observed, test string, methods ...string) {
		for _, method := range methods {
			ledger = append(ledger, CompatibilityCapability{
				ID:            rpcCapabilityID(service, method),
				Support:       support,
				Verification:  verification,
				LocalContract: contract,
				Observed:      observed,
				OwningTest:    test,
			})
		}
	}
	addFields := func(message string, support CompatibilitySupportLevel, verification CompatibilityVerification, contract, observed, test string, fields ...string) {
		for _, field := range fields {
			ledger = append(ledger, CompatibilityCapability{
				ID:            fieldCapabilityID(message, field),
				Support:       support,
				Verification:  verification,
				LocalContract: contract,
				Observed:      observed,
				OwningTest:    test,
			})
		}
	}

	const (
		dataService     = "google.bigtable.v2.Bigtable"
		tableService    = "google.bigtable.admin.v2.BigtableTableAdmin"
		instanceService = "google.bigtable.admin.v2.BigtableInstanceAdmin"
		operations      = "google.longrunning.Operations"
	)

	addRPCs(dataService, CompatibilityLocallySimplified, CompatibilityKnownNonconformant,
		"Table-targeted row reads and samples use the deterministic single-process store; production chunking, request statistics, and view targets are outside this baseline.",
		"Official-client table reads pass, but ReadRows omits production chunking/stats/target semantics and SampleRowKeys currently ignores row_range.",
		"TestGoogleDocs_ReadingData",
		"ReadRows", "SampleRowKeys")
	addRPCs(dataService, CompatibilityLocallySimplified, CompatibilityKnownNonconformant,
		"Table-targeted mutations use the local single-process store; Phase 1 owns rollback-safe failure semantics and target validation.",
		"Official-client success paths pass, but a later invalid operation can leave an earlier operation applied and unsupported targets are not rejected.",
		"TestGoogleDocs_WritingData",
		"MutateRow", "MutateRows", "CheckAndMutateRow", "ReadModifyWriteRow")
	addRPCs(dataService, CompatibilityLocallySimplified, CompatibilityDeclaredUnverified,
		"Returns success as a deterministic local no-op because there is no distributed metadata cache to warm.",
		"The handler returns success as a source-verified no-op; no focused client test owns the behavior yet.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"PingAndWarm")
	addRPCs(dataService, CompatibilityExplicitlyUnsupported, CompatibilityKnownNonconformant,
		"Change-stream serving is not advertised until the transactional outbox and resume contract pass Phase 4B conformance.",
		"The current handlers can return successful partial streams despite this advertised unsupported disposition.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"GenerateInitialChangeStreamPartitions", "ReadChangeStream")
	addRPCs(dataService, CompatibilityExplicitlyUnsupported, CompatibilityDeclaredUnverified,
		"GoogleSQL preparation and execution require the gated Phase 5 query engine.",
		"The handlers explicitly return Unimplemented; a focused client test is pending.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"PrepareQuery", "ExecuteQuery")
	addRPCs(dataService, CompatibilityExplicitlyUnsupported, CompatibilityDeclaredUnverified,
		"The session protocol is not advertised without a pinned client-level requirement and conformance suite.",
		"The embedded generated server returns Unimplemented; a pinned session client test is intentionally deferred.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"GetClientConfiguration", "OpenTable", "OpenAuthorizedView", "OpenMaterializedView")

	addRPCs(tableService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Table and column-family metadata and row deletion operate against one local persistent store; managed partitioning and replication are not simulated.",
		"Focused and official-client tests verify the narrow create/list/get/delete, column-family, and row-range deletion contract.",
		"TestGoogleDocs_TableManagement",
		"CreateTable", "ListTables", "GetTable", "DeleteTable", "ModifyColumnFamilies", "DropRowRange")
	addRPCs(tableService, CompatibilityLocallySimplified, CompatibilityKnownNonconformant,
		"Supported table update-mask paths should persist across restart and unsupported paths should fail without mutation.",
		"Selected in-memory updates pass, but policy fields are not durable and the complete current mask contract is not implemented.",
		"TestUpdateTable",
		"UpdateTable")
	addRPCs(tableService, CompatibilityLocallySimplified, CompatibilityDeclaredUnverified,
		"Consistency tokens deterministically describe the single local committed store and do not model replica catch-up.",
		"Handlers return a fixed local token contract; no focused client test owns it yet.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"GenerateConsistencyToken", "CheckConsistency")
	addRPCs(tableService, CompatibilityExplicitlyUnsupported, CompatibilityDeclaredUnverified,
		"Table tombstone retention and undelete semantics are not implemented.",
		"The handler explicitly returns Unimplemented; a focused client test is pending.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"UndeleteTable")
	addRPCs(tableService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Authorized-view descriptors have local metadata CRUD; Data RPC access enforcement is deferred to Phase 3.",
		"Focused CRUD tests verify the metadata-only contract; they do not claim Data RPC enforcement.",
		"TestAuthorizedViews_CRUD",
		"CreateAuthorizedView", "ListAuthorizedViews", "GetAuthorizedView", "UpdateAuthorizedView", "DeleteAuthorizedView")
	addRPCs(tableService, CompatibilityNotApplicable, CompatibilityDeclaredUnverified,
		"Deprecated snapshot RPCs are not simulated; immutable backups are the planned local snapshot contract.",
		"The handlers explicitly return Unimplemented; focused legacy-client coverage is not planned.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"CreateTableFromSnapshot", "SnapshotTable", "GetSnapshot", "ListSnapshots", "DeleteSnapshot")
	addRPCs(tableService, CompatibilityExplicitlyUnsupported, CompatibilityKnownNonconformant,
		"Data-bearing backup create, copy, and restore are not advertised until immutable schema and row snapshots exist.",
		"Current handlers can return metadata-only success without preserving immutable schema and row data.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"CreateBackup", "RestoreTable", "CopyBackup")
	addRPCs(tableService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Backup descriptor metadata can be retrieved, updated, listed, and deleted; it is not a data-protection guarantee.",
		"Focused tests verify descriptor metadata CRUD only.",
		"TestBackups_CRUD",
		"GetBackup", "UpdateBackup", "DeleteBackup", "ListBackups")
	addRPCs(tableService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"IAM policies round-trip for local tooling while serving remains intentionally unauthenticated and permissive.",
		"Focused tests verify permissive in-memory policy round trips.",
		"TestIAMStubs_Permissive",
		"GetIamPolicy", "SetIamPolicy", "TestIamPermissions")
	addRPCs(tableService, CompatibilityExplicitlyUnsupported, CompatibilityDeclaredUnverified,
		"Schema-bundle descriptors are not implemented in the Phase 0 server.",
		"The embedded generated server returns Unimplemented; focused CRUD tests arrive with Phase 2.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"CreateSchemaBundle", "UpdateSchemaBundle", "GetSchemaBundle", "ListSchemaBundles", "DeleteSchemaBundle")

	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityDeclaredUnverified,
		"Instance, cluster, and app-profile descriptors persist for local bootstrap tooling; capacity, replication, routing, and asynchronous managed lifecycle are not simulated.",
		"Instance persistence and cbt create/delete paths are tested; the grouped cluster/app-profile surface is not yet exhaustively client-verified.",
		"TestInstancePersistence",
		"CreateInstance", "GetInstance", "ListInstances", "UpdateInstance", "PartialUpdateInstance", "DeleteInstance",
		"CreateCluster", "GetCluster", "ListClusters", "UpdateCluster", "PartialUpdateCluster", "DeleteCluster",
		"CreateAppProfile", "GetAppProfile", "ListAppProfiles", "UpdateAppProfile", "DeleteAppProfile")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"IAM policies round-trip for local tooling while serving remains intentionally unauthenticated and permissive.",
		"Focused tests verify permissive in-memory policy round trips.",
		"TestIAMStubs_Permissive",
		"GetIamPolicy", "SetIamPolicy", "TestIamPermissions")
	addRPCs(instanceService, CompatibilityNotApplicable, CompatibilityDeclaredUnverified,
		"Hot-tablet analytics is managed observability and has no deterministic single-process analogue.",
		"The handler explicitly returns Unimplemented; focused managed-observability coverage is not planned.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"ListHotTablets")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Logical-view descriptors have local metadata CRUD; SQL execution is explicitly outside this contract.",
		"Focused tests verify the descriptor metadata CRUD contract.",
		"TestLogicalViews_CRUD",
		"CreateLogicalView", "GetLogicalView", "ListLogicalViews", "UpdateLogicalView", "DeleteLogicalView")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Materialized-view descriptors and the existing narrow local projection are compatibility-only; general GoogleSQL view semantics are not advertised.",
		"Focused tests verify create/get/list/delete descriptor behavior and the narrow custom projection only.",
		"TestCreateMaterializedViewRPC",
		"CreateMaterializedView", "GetMaterializedView", "ListMaterializedViews", "DeleteMaterializedView")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Materialized-view descriptor updates support the tested local update-mask path; general GoogleSQL view semantics are not advertised.",
		"A focused deletion-protection test verifies the update and subsequent descriptor read.",
		"TestDeletionProtection_UpdateThenDelete",
		"UpdateMaterializedView")

	addRPCs(operations, CompatibilityLocallySimplified, CompatibilityDeclaredUnverified,
		"Get performs an in-memory lookup only; durable operation registration is deferred to Phase 2.",
		"The method performs an in-memory lookup, but returned admin operations are not inserted and no client test verifies the path.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"GetOperation")
	addRPCs(operations, CompatibilityExplicitlyUnsupported, CompatibilityDeclaredUnverified,
		"Durable list, wait, deletion, and cancellation lifecycle semantics are not implemented in Phase 0.",
		"The embedded generated server returns Unimplemented; focused lifecycle tests arrive with Phase 2.",
		"TestCompatibilityLedgerCoversRegisteredRPCs",
		"ListOperations", "DeleteOperation", "CancelOperation", "WaitOperation")

	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Table names and row sets implement the documented local key/range subset.",
		"A focused handler test exercises table-targeted keys and ranges.",
		"TestReadRows",
		"table_name", "rows")
	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Filters implement the audited local filter subset.",
		"Official-client filter tests exercise the field through ReadRows.",
		"TestGoogleDocs_Filters",
		"filter")
	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Row limits and reverse ordering implement the documented local read subset.",
		"Official-client tests exercise limited and reversed ReadRows scans.",
		"TestGoogleDocs_ReadingData",
		"rows_limit", "reversed")
	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilityExplicitlyUnsupported, CompatibilityKnownNonconformant,
		"View targets, app-profile routing, and request statistics must be rejected until their owning conformance phases land.",
		"The current handler ignores or incompletely handles these fields instead of rejecting them.",
		"TestCompatibilityLedgerFieldsAreExhaustive",
		"authorized_view_name", "materialized_view_name", "app_profile_id", "request_stats_view")
	addFields("google.bigtable.v2.SampleRowKeysRequest", CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Table-targeted sampling returns deterministic local sample boundaries.",
		"Focused tests exercise the table_name target and deterministic sample response.",
		"TestSampleRowKeys",
		"table_name")
	addFields("google.bigtable.v2.SampleRowKeysRequest", CompatibilityExplicitlyUnsupported, CompatibilityKnownNonconformant,
		"View targets, app-profile routing, and range-restricted sampling must be rejected until their owning conformance phases land.",
		"The current handler ignores these target and range fields instead of rejecting them.",
		"TestCompatibilityLedgerFieldsAreExhaustive",
		"authorized_view_name", "materialized_view_name", "app_profile_id", "row_range")

	addFields("google.bigtable.v2.MutateRowRequest", CompatibilityLocallySimplified, CompatibilityKnownNonconformant,
		"Table-targeted mutations implement the local success-path subset; Phase 1 owns rollback-safe failure behavior.",
		"Successful mutations are tested, but a later invalid operation can leave an earlier operation applied.",
		"TestGoogleDocs_WritingData",
		"table_name", "row_key", "mutations")
	addFields("google.bigtable.v2.MutateRowRequest", CompatibilityExplicitlyUnsupported, CompatibilityKnownNonconformant,
		"Authorized-view targeting, app-profile routing, and idempotency tokens must be rejected until implemented.",
		"The current handler ignores these fields instead of rejecting them.",
		"TestCompatibilityLedgerFieldsAreExhaustive",
		"authorized_view_name", "app_profile_id", "idempotency")

	addFields("google.bigtable.v2.MutateRowsRequest", CompatibilityLocallySimplified, CompatibilityKnownNonconformant,
		"Table-targeted bulk entries implement the local success-path subset; Phase 1 owns per-entry rollback-safe failures.",
		"Successful entries are tested, but mixed invalid operations do not yet satisfy the required failure contract.",
		"TestGoogleDocs_WritingData",
		"table_name", "entries")
	addFields("google.bigtable.v2.MutateRowsRequest", CompatibilityExplicitlyUnsupported, CompatibilityKnownNonconformant,
		"Authorized-view targeting and app-profile routing must be rejected until implemented.",
		"The current handler ignores these fields instead of rejecting them.",
		"TestCompatibilityLedgerFieldsAreExhaustive",
		"authorized_view_name", "app_profile_id")

	addFields("google.bigtable.v2.CheckAndMutateRowRequest", CompatibilityLocallySimplified, CompatibilityKnownNonconformant,
		"Table-targeted predicate and branch mutations implement the local success-path subset; Phase 1 owns atomic failure behavior.",
		"Predicate branches are tested, but unsupported targets and complete rollback semantics are not conformant.",
		"TestCheckAndMutateRowWithPredicate",
		"table_name", "row_key", "predicate_filter", "true_mutations", "false_mutations")
	addFields("google.bigtable.v2.CheckAndMutateRowRequest", CompatibilityExplicitlyUnsupported, CompatibilityKnownNonconformant,
		"Authorized-view targeting and app-profile routing must be rejected until implemented.",
		"The current handler ignores these fields instead of rejecting them.",
		"TestCompatibilityLedgerFieldsAreExhaustive",
		"authorized_view_name", "app_profile_id")

	addFields("google.bigtable.v2.ReadModifyWriteRowRequest", CompatibilityLocallySimplified, CompatibilityKnownNonconformant,
		"Table-targeted read-modify-write rules implement the local success-path subset; Phase 1 owns atomic failure behavior.",
		"Successful rules are covered by client tests, but unsupported targets and complete rollback semantics are not conformant.",
		"TestGoogleDocs_WritingData",
		"table_name", "row_key", "rules")
	addFields("google.bigtable.v2.ReadModifyWriteRowRequest", CompatibilityExplicitlyUnsupported, CompatibilityKnownNonconformant,
		"Authorized-view targeting and app-profile routing must be rejected until implemented.",
		"The current handler ignores these fields instead of rejecting them.",
		"TestCompatibilityLedgerFieldsAreExhaustive",
		"authorized_view_name", "app_profile_id")

	addFields("google.bigtable.admin.v2.CreateTableRequest", CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Parent, table ID, and supported table schema fields create local persistent table metadata.",
		"A focused handler test verifies the complete supported create request and returned family metadata.",
		"TestCreateTableWithFamily",
		"parent", "table_id", "table")
	addFields("google.bigtable.admin.v2.CreateTableRequest", CompatibilityExplicitlyUnsupported, CompatibilityKnownNonconformant,
		"Initial splits require a local partition model and must not be silently accepted.",
		"The current handler accepts and ignores initial_splits.",
		"TestCompatibilityLedgerFieldsAreExhaustive",
		"initial_splits")
	addFields("google.bigtable.admin.v2.ColumnFamily", CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Garbage-collection rules are retained as local column-family metadata.",
		"A focused create/get test verifies garbage-collection rule metadata round trips.",
		"TestCreateTableWithFamily",
		"gc_rule")
	addFields("google.bigtable.admin.v2.ColumnFamily", CompatibilityExplicitlyUnsupported, CompatibilityKnownNonconformant,
		"Aggregate value types require the gated typed aggregate engine.",
		"The current handler accepts or preserves value_type without implementing typed aggregate semantics.",
		"TestCompatibilityLedgerFieldsAreExhaustive",
		"value_type")

	return ledger
}

var compatibilityLedger = buildCompatibilityLedger()
