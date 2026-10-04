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
// the declared support contract. A test_verified capability names a passing
// conformance test as its OwningTest; known gaps stay visible as
// known_nonconformant, and declared_unverified marks contracts no test owns yet.
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

	const (
		ledgerStructureTest = "TestCompatibilityLedgerCoversRegisteredRPCs"
		fieldStructureTest  = "TestCompatibilityLedgerFieldsAreExhaustive"
	)

	// --- google.bigtable.v2.Bigtable: reads ---
	addRPCs(dataService, CompatibilitySupported, CompatibilityTestVerified,
		"ReadRows resolves exactly one target, validates filters and limits before reading, applies read-time GC to copies, enforces authorized-view subsets, and streams production-shaped chunks: row key on the first chunk of a row, family/qualifier only when they change, timestamp/labels on the first piece, and large values split with value_size.",
		"Chunk shape, value splitting with official-client reconstruction, REQUEST_STATS_FULL statistics, reversed scans with limits, negative-limit rejection and key/range de-duplication are verified; last_scanned_row_key is not emitted (optional in the API).",
		"TestConformanceReadChunkShape",
		"ReadRows")
	addRPCs(dataService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"SampleRowKeys honors row_range and returns the table's initial split keys plus one sample every 512KiB of stored data, ending with the range end key or the empty end-of-table key carrying the total offset.",
		"Sample positions approximate tablet boundaries deterministically; range restriction, split keys, end-key semantics and monotonic offsets are verified.",
		"TestConformanceSampleRowKeysEndOfTable",
		"SampleRowKeys")

	// --- google.bigtable.v2.Bigtable: writes ---
	addRPCs(dataService, CompatibilitySupported, CompatibilityTestVerified,
		"All mutations are validated before any is applied; a failing request leaves the row and the change stream untouched. Unknown families are NotFound, invalid timestamps InvalidArgument, and storage context errors map to Canceled/DeadlineExceeded.",
		"Atomic rejection of a valid-then-invalid mutation list, the absence of change-stream records for rejected writes and storage error mapping are verified.",
		"TestConformanceWriteMutateRowAtomicity",
		"MutateRow")
	addRPCs(dataService, CompatibilitySupported, CompatibilityTestVerified,
		"Entries commit in one transaction with per-entry statuses: invalid entries report their own code and are not applied, valid entries are applied, and duplicate row keys in one batch apply in request order.",
		"Mixed valid/invalid batches and in-order duplicate keys are verified.",
		"TestConformanceWriteMutateRowsPerEntryStatus",
		"MutateRows")
	addRPCs(dataService, CompatibilitySupported, CompatibilityTestVerified,
		"The predicate is evaluated on the (view-restricted) row and only the chosen branch is validated and applied atomically; transactional routing is required for registered app profiles.",
		"Atomic rejection of an invalid chosen branch is verified.",
		"TestConformanceWriteCheckAndMutateRowAtomicity",
		"CheckAndMutateRow")
	addRPCs(dataService, CompatibilitySupported, CompatibilityTestVerified,
		"All rules are validated before any is applied; increments require 8-byte big-endian values (FailedPrecondition otherwise) and aggregate families are rejected with InvalidArgument.",
		"Atomic rejection of a valid-then-invalid rule list is verified.",
		"TestConformanceWriteReadModifyWriteRowAtomicity",
		"ReadModifyWriteRow")
	addRPCs(dataService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Validates the instance name, the instance when strict admin mode is on, and the app profile; there is no distributed metadata cache to warm, so success is a no-op.",
		"Name and app-profile validation are verified.",
		"TestConformanceTargetAppProfileResolution",
		"PingAndWarm")

	// --- google.bigtable.v2.Bigtable: change streams ---
	addRPCs(dataService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Tables with change_stream_config expose one full-keyspace partition (single local tablet); tables without it are FailedPrecondition.",
		"Partition generation is verified; partition splits and merges are never produced.",
		"TestConformanceChangeStreamPartitions",
		"GenerateInitialChangeStreamPartitions")
	addRPCs(dataService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Records come from a transactional outbox written with each row commit: one DataChange per commit with grouped chunks, USER and GARBAGE_COLLECTION types, DeleteFromRow expanded to DeleteFromFamily per family, opaque continuation tokens, heartbeats with low watermarks, retention-checked start_time and CloseStream OK at end_time.",
		"Grouping, GC records, DropRowRange records, resume without loss or duplication, start_time validation, heartbeats, end_time close and purge-on-disable are verified; only one partition exists, so client split/merge handling is not exercised.",
		"TestConformanceChangeStreamMutateRowIsOneDataChange",
		"ReadChangeStream")

	// --- google.bigtable.v2.Bigtable: GoogleSQL ---
	addRPCs(dataService, CompatibilitySupported, CompatibilityTestVerified,
		"GoogleSQL for Bigtable queries are parsed, type-checked and executed by the local engine; prepared queries are self-describing and report PREPARED_QUERY_EXPIRED after schema changes. Window, geography, approximate-quantile functions and unsupported types return Unimplemented.",
		"The official Go client prepares, binds and executes parameterized, aggregated and limited queries, and transparently re-prepares after a schema change.",
		"TestConformanceSQLQueryThroughClient",
		"PrepareQuery", "ExecuteQuery")

	// --- google.bigtable.v2.Bigtable: sessions ---
	addRPCs(dataService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Reports session_load 0 and asks clients to stop polling; there is no fleet whose load could be balanced.",
		"The returned configuration is verified.",
		"TestConformanceSessionGetClientConfiguration",
		"GetClientConfiguration")
	addRPCs(dataService, CompatibilitySupported, CompatibilityTestVerified,
		"Sessions validate the target when opened (NotFound for missing tables or views), enforce READ/WRITE permissions, and route virtual ReadRow/MutateRow calls through the same pipeline as the unary RPCs; per-call failures arrive as SessionResponse errors while the stream stays open.",
		"Missing-target errors, read-only sessions rejecting writes without closing the stream, and authorized-view restrictions are verified.",
		"TestConformanceSessionPermissionsAndErrors",
		"OpenTable")
	addRPCs(dataService, CompatibilitySupported, CompatibilityTestVerified,
		"Authorized-view sessions apply the view's row prefixes and family subsets to every virtual ReadRow/MutateRow.",
		"Out-of-view reads are filtered and out-of-view writes return PermissionDenied session errors.",
		"TestConformanceSessionAuthorizedViewRestrictions",
		"OpenAuthorizedView")
	addRPCs(dataService, CompatibilitySupported, CompatibilityTestVerified,
		"Materialized-view sessions open only on existing views, serve up-to-date view rows and reject writes.",
		"A session reads a view row and receives PERMISSION_DENIED for a mutation while staying open.",
		"TestConformanceMaterializedViewSession",
		"OpenMaterializedView")

	// --- google.bigtable.admin.v2.BigtableTableAdmin: tables ---
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"CreateTable validates IDs, GC rules (max_age >= 1ms, <= 500 bytes), aggregate value types, change-stream retention and row-key schemas, and persists initial splits; GetTable/ListTables honor views and pagination.",
		"Views, pagination, validation and restart persistence are verified.",
		"TestConformanceTableAdminCreateTableValidation",
		"CreateTable")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"GetTable defaults to SCHEMA_VIEW and supports NAME_ONLY, REPLICATION_VIEW and ENCRYPTION_VIEW (one READY cluster, GOOGLE_DEFAULT_ENCRYPTION) and FULL.",
		"Every view and NotFound are verified.",
		"TestConformanceTableAdminGetTableViews",
		"GetTable")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"ListTables defaults to NAME_ONLY, supports SCHEMA_VIEW, and paginates with stable tokens; invalid tokens are InvalidArgument.",
		"Views and pagination are verified.",
		"TestConformanceTableAdminListTablesViewsAndPagination",
		"ListTables")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"Supported mask paths (change_stream_config[.retention_period], deletion_protection, row_key_schema, automated_backup_policy[.*], tiered_storage_config) persist across restart and return an UpdateTableMetadata LRO; column_families is Unimplemented and '*' is InvalidArgument; disabling the change stream purges its records.",
		"Each mask path, restart persistence and rejected paths are verified.",
		"TestConformanceTableAdminUpdateTableMasks",
		"UpdateTable")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"DeleteTable is blocked by table/authorized-view deletion protection and soft-deletes the table; UndeleteTable restores data within 7 days, enables deletion protection, and is AlreadyExists when a live table with the same ID exists.",
		"Restore, protection, conflict and not-found paths are verified.",
		"TestConformanceTableAdminDeleteUndelete",
		"DeleteTable", "UndeleteTable")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"All modifications are validated before any is applied; per-modification update_mask supports gc_rule, value_type is immutable, and dropping a family physically removes its data.",
		"Atomicity, immutable value types and no resurrection of dropped data are verified.",
		"TestConformanceTableAdminModifyColumnFamiliesAtomic",
		"ModifyColumnFamilies")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"Deletes by prefix or the whole table in one transaction, recording DeleteFromFamily change records when change streams are enabled.",
		"Prefix and full-table deletion are verified.",
		"TestConformanceTableAdminDropRowRange",
		"DropRowRange")
	addRPCs(tableService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Tokens are validated per table and are always consistent because there is a single local replica.",
		"Valid, malformed and cross-table tokens are verified.",
		"TestConformanceTableAdminConsistencyTokens",
		"GenerateConsistencyToken", "CheckConsistency")
	addRPCs(tableService, CompatibilityExplicitlyUnsupported, CompatibilityTestVerified,
		"Deprecated private-alpha snapshot RPCs answer Unimplemented; backups are the supported snapshot mechanism.",
		"Every snapshot RPC is verified to return Unimplemented.",
		"TestConformanceTableAdminSnapshotsUnimplemented",
		"CreateTableFromSnapshot", "SnapshotTable", "GetSnapshot", "ListSnapshots", "DeleteSnapshot")

	// --- google.bigtable.admin.v2.BigtableTableAdmin: authorized views ---
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"CRUD with etags (ABORTED on mismatch), NAME_ONLY/BASIC/FULL views, pagination, subset_view/deletion_protection masks and typed LRO metadata; data RPCs through a view are restricted to its row prefixes and family subsets.",
		"Views, etags, deletion protection and data-path restrictions are verified.",
		"TestConformanceAuthorizedViewResponseViewsAndPagination",
		"CreateAuthorizedView", "ListAuthorizedViews", "GetAuthorizedView")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"Updates honor update_mask (empty = fields set in the request, '*' = all) and etags; deletes honor etags and deletion protection. Deleting a table deletes its views.",
		"Stale etags (ABORTED), mask handling and deletion protection are verified.",
		"TestConformanceAuthorizedViewEtagAndUpdateMask",
		"UpdateAuthorizedView", "DeleteAuthorizedView")

	// --- google.bigtable.admin.v2.BigtableTableAdmin: backups ---
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"Backups are immutable schema and row snapshots: expire_time 6h..90d (365d for Enterprise Plus), HOT/STANDARD rules, per-table limits, copy rules (no copy of a copy, <= 30 days after source creation) and restore into a new table without inheriting GC policies.",
		"expire_time validation (including the Enterprise Plus 365 day limit), HOT rules and per-table limits are verified.",
		"TestConformanceBackupExpireTimeAndEdition",
		"CreateBackup")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"RestoreTable creates a new table from the backup snapshot, independent of the live source, with RestoreInfo and an OptimizeRestoredTable operation; GC policies, automated backup policy and deletion protection are not inherited.",
		"Restore after deleting the source table and restarting the server reproduces the exact rows and families.",
		"TestConformanceBackupRestoreAfterSourceDeletedAndRestart",
		"RestoreTable")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"CopyBackup requires a READY source that is not itself a copy, always produces a STANDARD backup, and limits expire_time to 6h..30d from the request; a source expiring within 24h is FailedPrecondition.",
		"Copy rules and copy survival after deleting the source table and original backup are verified.",
		"TestConformanceBackupCopyRules",
		"CopyBackup", "DeleteBackup")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"ListBackups supports the '-' cluster wildcard, filters (name, source_table, state, backup_type, source_backup, times, size_bytes with comparison operators and AND), single-field order_by (default start_time desc) and pagination.",
		"Filtering, ordering, wildcard and pagination are verified; OR/NOT filters and multi-field order_by are rejected.",
		"TestConformanceBackupListFilterOrderPagination",
		"ListBackups")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"Expired backups are invisible to Get/List/Copy/Restore/Update and are purged by maintenance with their manifests and snapshot rows.",
		"Hiding and purge of expired backups are verified.",
		"TestConformanceBackupExpiredHiddenAndPurged",
		"GetBackup")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"UpdateBackup validates expire_time with the create rules and allows hot_to_standard_time changes only on HOT backups; backup_type is immutable.",
		"Expiry and HOT-backup update rules are verified.",
		"TestConformanceBackupHotBackupRules",
		"UpdateBackup")

	// --- google.bigtable.admin.v2.BigtableTableAdmin: IAM and schema bundles ---
	addRPCs(tableService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Policies persist with etags and update_mask, require existing resources and are removed with the table; TestIamPermissions grants every permission because the emulator is unauthenticated.",
		"NotFound, etag, update_mask, restart persistence and cleanup are verified; IAM is not enforced on data or admin calls.",
		"TestConformanceIAMOfficialClientHandles",
		"GetIamPolicy", "SetIamPolicy", "TestIamPermissions")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"Proto (FileDescriptorSet) and Avro (JSON) bundles with validation, 10 per table, 4MB, immutable oneof structure, backward-compatibility checks overridable by ignore_warnings, etags and pagination.",
		"CRUD, invalid descriptors, limits, compatibility checks and etags are verified.",
		"TestConformanceSchemaBundleProtoCRUD",
		"CreateSchemaBundle", "UpdateSchemaBundle", "GetSchemaBundle", "DeleteSchemaBundle")
	addRPCs(tableService, CompatibilitySupported, CompatibilityTestVerified,
		"ListSchemaBundles paginates per table with stable tokens.",
		"Pagination and invalid tokens are verified.",
		"TestConformanceSchemaBundlePagination",
		"ListSchemaBundles")

	// --- google.bigtable.admin.v2.BigtableInstanceAdmin ---
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Instances need a 6-33 character ID and at least one cluster; display names 4-30 characters; PartialUpdateInstance supports display_name/labels/type/edition and rejects PRODUCTION->DEVELOPMENT; deletion is blocked by protected resources or backups and cascades otherwise. Capacity and replication are not simulated.",
		"ID, cluster-count and display-name validation and the created default app profile are verified.",
		"TestConformanceInstanceAdminCreateRequirements",
		"CreateInstance", "GetInstance")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"ListInstances/ListClusters return one page (the proto has no page_size) and validate page tokens; ListAppProfiles paginates; '-' lists across instances.",
		"Pagination, token validation and the '-' wildcard are verified.",
		"TestConformanceInstanceAdminListPaginationAndWildcard",
		"ListInstances", "ListClusters", "ListAppProfiles")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"UpdateInstance changes only display_name and type (PRODUCTION->DEVELOPMENT rejected); labels are left unchanged.",
		"Update scope and type downgrade rejection are verified.",
		"TestConformanceInstanceAdminUpdateInstanceScope",
		"UpdateInstance")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"PartialUpdateInstance supports display_name, labels, type and edition masks and returns a PartialUpdateInstance LRO.",
		"Every mask path and invalid paths are verified.",
		"TestConformanceInstanceAdminPartialUpdateMasks",
		"PartialUpdateInstance")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"DeleteInstance is blocked by protected tables, views and backups, and otherwise cascades to every child resource.",
		"Deletion through the official client is verified; blocking rules are verified by TestConformanceAuthorizedViewDeletionProtection and TestConformanceBackupBlocksClusterAndInstanceDeletion.",
		"TestConformanceInstanceAdminOfficialClient",
		"DeleteInstance")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Cluster IDs are validated, serve_nodes/autoscaling/node_scaling_factor are stored, and DeleteCluster is blocked for the last cluster, for clusters holding backups and for clusters referenced by app-profile routing. Node capacity has no effect locally.",
		"Cluster ID validation and lookup are verified.",
		"TestConformanceInstanceAdminClusterIDValidation",
		"CreateCluster", "GetCluster")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"UpdateCluster stores serve_nodes; PartialUpdateCluster supports serve_nodes, cluster_config.cluster_autoscaling_config and node_scaling_factor masks. Capacity has no local effect.",
		"Update paths and invalid masks are verified.",
		"TestConformanceInstanceAdminClusterUpdates",
		"UpdateCluster", "PartialUpdateCluster")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"DeleteCluster is blocked for the last cluster, for clusters holding unexpired backups and for clusters referenced by single-cluster app-profile routing.",
		"Each blocking rule is verified.",
		"TestConformanceInstanceAdminDeleteClusterRules",
		"DeleteCluster")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"App profiles must route to existing clusters and carry etags; a default single-cluster transactional profile is created with each instance. Routing affects validation (Data Boost read-only, transactional writes) but not placement.",
		"Routing validation against existing clusters is verified.",
		"TestConformanceInstanceAdminAppProfileRouting",
		"CreateAppProfile", "DeleteAppProfile")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"App profiles carry etags; UpdateAppProfile honors update_mask and etags and returns an UpdateAppProfile LRO.",
		"Etag round trips and mismatches are verified.",
		"TestConformanceInstanceAdminAppProfileEtag",
		"GetAppProfile", "UpdateAppProfile")
	addRPCs(instanceService, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Instance policies persist with etags and update_mask and require an existing instance; TestIamPermissions grants every permission because the emulator is unauthenticated.",
		"NotFound, etag and persistence behavior are verified; IAM is not enforced.",
		"TestConformanceIAMOfficialClientHandles",
		"GetIamPolicy", "SetIamPolicy", "TestIamPermissions")
	addRPCs(instanceService, CompatibilityNotApplicable, CompatibilityTestVerified,
		"Hot tablets and memory layers describe production serving hardware; there is no single-process analogue, so these RPCs answer Unimplemented.",
		"Unimplemented responses are verified.",
		"TestConformanceInstanceAdminProductionHardwareUnimplemented",
		"ListHotTablets", "GetMemoryLayer", "ListMemoryLayers", "UpdateMemoryLayer")
	addRPCs(instanceService, CompatibilitySupported, CompatibilityTestVerified,
		"Logical-view descriptors support CRUD, etags, deletion protection, parent-scoped pagination and typed LRO metadata; the query is validated and executed by the GoogleSQL engine (a stub in the default build).",
		"Descriptor CRUD is verified; query validation and execution belong to the GoogleSQL suite.",
		"TestLogicalViews_CRUD",
		"CreateLogicalView", "GetLogicalView", "ListLogicalViews", "UpdateLogicalView", "DeleteLogicalView")
	addRPCs(instanceService, CompatibilitySupported, CompatibilityTestVerified,
		"Continuous materialized views are defined in GoogleSQL (GROUP BY or ORDER BY), populated at creation, refreshed before reads after source writes, read-only, persisted, deletion protected and immutable except for deletion_protection. Multi-column keys use OrderedCodeBytes struct encoding (an emulator choice).",
		"Client and handler tests create, read, refresh, list, protect, update and delete views and reload them after a restart.",
		"TestConformanceMaterializedViewSurvivesRestart",
		"CreateMaterializedView", "GetMaterializedView", "ListMaterializedViews", "UpdateMaterializedView", "DeleteMaterializedView")

	// --- google.longrunning.Operations ---
	addRPCs(operations, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Every LRO-returning admin RPC registers a durable operation (operations/<resource>/locations/local/operations/<n>) with its documented metadata type. Local operations complete synchronously, so Wait returns immediately and Cancel is a no-op for done operations; List supports prefix, done=true|false filters and pagination.",
		"Registration and metadata types for every LRO-returning RPC (materialized views excluded in the stub build) are verified.",
		"TestConformanceOperationsMetadataTypes",
		"GetOperation")
	addRPCs(operations, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"ListOperations filters by name prefix and done=true|false, paginates, rejects other filters and answers return_partial_success with Unimplemented.",
		"Filters, pagination and restart durability are verified.",
		"TestConformanceOperationsListFilterPagination",
		"ListOperations")
	addRPCs(operations, CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Wait returns the completed operation immediately, Delete removes it, and Cancel is a no-op for done operations.",
		"Wait, Delete and Cancel are verified.",
		"TestConformanceOperationsWaitDeleteCancel",
		"WaitOperation", "DeleteOperation", "CancelOperation")

	// --- Pinned request fields ---
	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Table targets and row sets with overlapping keys and ranges are merged and de-duplicated in key order.",
		"Mixed key/range de-duplication and ordering are verified.",
		"TestConformanceReadMixedKeysAndRanges",
		"table_name", "rows")
	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Filters are validated before reading and evaluated without mutating stored data, including Interleave duplicates, Sink, ValueBitmask and label rules.",
		"Interleave, Sink, ValueBitmask, label validation and storage immutability are verified.",
		"TestConformanceFilterSinkDocExample",
		"filter")
	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Negative rows_limit is InvalidArgument; reversed scans return rows in descending order and honor the limit.",
		"Reversed scans with limits and negative-limit rejection are verified.",
		"TestConformanceReadReversedWithLimit",
		"rows_limit", "reversed")
	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilitySupported, CompatibilityTestVerified,
		"REQUEST_STATS_FULL sends a final response holding only request_stats; NONE sends none.",
		"Request statistics are verified.",
		"TestConformanceReadRequestStatsFull",
		"request_stats_view")
	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Reads through an authorized view return only rows matching its row prefixes and cells in its family subsets.",
		"Row, family, qualifier and qualifier-prefix restrictions are verified.",
		"TestConformanceAuthorizedViewReadRestriction",
		"authorized_view_name")
	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilitySupported, CompatibilityTestVerified,
		"App profiles resolve against registered instances (NotFound when missing); Data Boost profiles reject reversed reads.",
		"Profile resolution and Data Boost restrictions are verified.",
		"TestConformanceTargetAppProfileResolution",
		"app_profile_id")
	addFields("google.bigtable.v2.ReadRowsRequest", CompatibilitySupported, CompatibilityTestVerified,
		"materialized_view_name reads the view's derived rows after bringing the view up to date.",
		"The official client reads and samples a view through OpenMaterializedView.",
		"TestConformanceMaterializedViewReadsAndRefresh",
		"materialized_view_name")

	addFields("google.bigtable.v2.SampleRowKeysRequest", CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Table-targeted sampling restricted to row_range returns split keys and 512KiB samples inside the range.",
		"Range restriction and end-key semantics are verified.",
		"TestConformanceSampleRowKeysRowRange",
		"table_name", "row_range")
	addFields("google.bigtable.v2.SampleRowKeysRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Exactly one of table_name, authorized_view_name or materialized_view_name is required; a missing view is NotFound.",
		"Target validation is verified.",
		"TestConformanceTargetExactlyOneTarget",
		"authorized_view_name")
	addFields("google.bigtable.v2.SampleRowKeysRequest", CompatibilitySupported, CompatibilityTestVerified,
		"App profiles resolve like other data RPCs (NotFound when missing on a registered instance).",
		"Profile resolution is verified.",
		"TestConformanceTargetAppProfileResolution",
		"app_profile_id")
	addFields("google.bigtable.v2.SampleRowKeysRequest", CompatibilitySupported, CompatibilityTestVerified,
		"materialized_view_name reads the view's derived rows after bringing the view up to date.",
		"The official client reads and samples a view through OpenMaterializedView.",
		"TestConformanceMaterializedViewReadsAndRefresh",
		"materialized_view_name")

	addFields("google.bigtable.v2.MutateRowRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Mutations are validated as a unit and applied atomically to the target row.",
		"Atomic rejection is verified.",
		"TestConformanceWriteMutateRowAtomicity",
		"table_name", "row_key", "mutations")
	addFields("google.bigtable.v2.MutateRowRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Tokens of at least 8 bytes de-duplicate retries within a 15 minute window; shorter tokens are rejected.",
		"De-duplication and short-token rejection are verified.",
		"TestConformanceWriteIdempotency",
		"idempotency")
	addFields("google.bigtable.v2.MutateRowRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Writes outside the authorized view are PermissionDenied (DeleteFromRow always; DeleteFromFamily unless the view grants the \"\" qualifier prefix).",
		"Out-of-view SetCell, DeleteFromRow and DeleteFromFamily are verified.",
		"TestConformanceAuthorizedViewWriteRestriction",
		"authorized_view_name")
	addFields("google.bigtable.v2.MutateRowRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Missing profiles on registered instances are NotFound and Data Boost profiles reject writes with FailedPrecondition.",
		"Profile resolution and Data Boost write rejection are verified.",
		"TestConformanceTargetAppProfileResolution",
		"app_profile_id")

	addFields("google.bigtable.v2.MutateRowsRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Entries are validated and applied independently with per-entry status codes in one transaction.",
		"Mixed batches and duplicate keys are verified.",
		"TestConformanceWriteMutateRowsPerEntryStatus",
		"table_name", "entries")
	addFields("google.bigtable.v2.MutateRowsRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Entries outside the authorized view fail individually with PermissionDenied.",
		"Per-entry PermissionDenied is verified.",
		"TestConformanceAuthorizedViewWriteRestriction",
		"authorized_view_name")
	addFields("google.bigtable.v2.MutateRowsRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Missing profiles on registered instances are NotFound and Data Boost profiles reject writes.",
		"Profile resolution is verified.",
		"TestConformanceTargetAppProfileResolution",
		"app_profile_id")

	addFields("google.bigtable.v2.CheckAndMutateRowRequest", CompatibilitySupported, CompatibilityTestVerified,
		"The predicate selects a branch; only that branch is validated and applied atomically.",
		"Branch atomicity is verified.",
		"TestConformanceWriteCheckAndMutateRowAtomicity",
		"table_name", "row_key", "predicate_filter", "true_mutations", "false_mutations")
	addFields("google.bigtable.v2.CheckAndMutateRowRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Through an authorized view the predicate only sees in-view data.",
		"Predicate isolation from out-of-view data is verified.",
		"TestConformanceAuthorizedViewCheckAndMutatePredicate",
		"authorized_view_name")
	addFields("google.bigtable.v2.CheckAndMutateRowRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Registered profiles must use single-cluster routing with transactional writes; multi-cluster profiles are FailedPrecondition.",
		"Multi-cluster rejection is verified.",
		"TestConformanceTargetAppProfileResolution",
		"app_profile_id")

	addFields("google.bigtable.v2.ReadModifyWriteRowRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Rules are validated as a unit and applied atomically.",
		"Atomic rejection is verified.",
		"TestConformanceWriteReadModifyWriteRowAtomicity",
		"table_name", "row_key", "rules")
	addFields("google.bigtable.v2.ReadModifyWriteRowRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Rules targeting cells outside the authorized view are PermissionDenied.",
		"Out-of-view rules are verified.",
		"TestConformanceAuthorizedViewReadModifyWrite",
		"authorized_view_name")
	addFields("google.bigtable.v2.ReadModifyWriteRowRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Registered profiles must use single-cluster routing with transactional writes; multi-cluster profiles are FailedPrecondition.",
		"Multi-cluster rejection is verified.",
		"TestConformanceTargetAppProfileResolution",
		"app_profile_id")

	addFields("google.bigtable.admin.v2.CreateTableRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Parent, table ID and table schema are validated and persisted.",
		"Validation and persistence are verified.",
		"TestConformanceTableAdminCreateTableValidation",
		"parent", "table_id", "table")
	addFields("google.bigtable.admin.v2.CreateTableRequest", CompatibilitySupported, CompatibilityTestVerified,
		"Initial splits must be non-empty, are persisted across restart and are reported by SampleRowKeys.",
		"Persistence across restart is verified.",
		"TestConformanceTableAdminInitialSplitsPersisted",
		"initial_splits")
	addFields("google.bigtable.admin.v2.ColumnFamily", CompatibilityLocallySimplified, CompatibilityTestVerified,
		"Nested union/intersection rules use production semantics; max_age below 1ms and rules over 500 bytes are rejected. Collection is eager: expired cells are dropped on the next write and hidden on read, whereas production collects asynchronously and may still return eligible cells.",
		"Union/intersection truth tables (unit and end-to-end), eager collection on write and size/age validation are verified (also TestConformanceGCRuleValidation).",
		"TestConformanceGCRuleTruthTables",
		"gc_rule")
	addFields("google.bigtable.admin.v2.ColumnFamily", CompatibilitySupported, CompatibilityTestVerified,
		"Aggregate types (sum/min/max over Int64; HLL++ needs the GoogleSQL build) are immutable after creation; AddToCell/MergeToCell require them and SetCell is rejected on them.",
		"Sum/min/max semantics and mutation-type validation are verified; immutability is verified by TestConformanceTableAdminValueTypeImmutable.",
		"TestConformanceWriteAggregateSemantics",
		"value_type")

	return ledger
}

var compatibilityLedger = buildCompatibilityLedger()
