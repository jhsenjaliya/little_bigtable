package bttest

import (
	"strings"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dataTarget is the resolved resource of a Data API request.
type dataTarget struct {
	tbl        *table
	view       *viewPolicy       // non-nil for authorized-view requests
	mv         *materializedView // non-nil for materialized-view reads
	appProfile *btapb.AppProfile // nil for unregistered instances
	resource   string            // the requested resource name, for errors
}

// resolveAppProfile returns the app profile a data request runs under. Data
// requests against an instance that was never registered (non-strict mode)
// accept any profile ID, matching the official emulator's lenient naming.
func (s *server) resolveAppProfile(instance, id string) (*btapb.AppProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[instance]; !ok {
		return nil, nil
	}
	if id == "" {
		id = "default"
	}
	p, ok := s.appProfiles[instance+"/appProfiles/"+id]
	if !ok {
		if id == "default" {
			return nil, nil
		}
		return nil, status.Errorf(codes.NotFound, "app profile %q not found in instance %q", id, instance)
	}
	return p, nil
}

func isDataBoostProfile(p *btapb.AppProfile) bool {
	return p != nil && p.GetDataBoostIsolationReadOnly() != nil
}

func singleTarget(names ...string) (string, error) {
	found := ""
	for _, n := range names {
		if n == "" {
			continue
		}
		if found != "" {
			return "", status.Error(codes.InvalidArgument, "exactly one of table_name, authorized_view_name or materialized_view_name must be set")
		}
		found = n
	}
	if found == "" {
		return "", status.Error(codes.InvalidArgument, "one of table_name, authorized_view_name or materialized_view_name must be set")
	}
	return found, nil
}

// resolveReadTarget resolves ReadRows and SampleRowKeys targets.
func (s *server) resolveReadTarget(tableName, avName, mvName, appProfileID string) (*dataTarget, error) {
	if _, err := singleTarget(tableName, avName, mvName); err != nil {
		return nil, err
	}
	var target *dataTarget
	var err error
	switch {
	case mvName != "":
		target, err = s.materializedViewTarget(mvName)
	case avName != "":
		target, err = s.authorizedViewTarget(avName)
	default:
		var tbl *table
		tbl, err = s.localRequireTable(tableName)
		target = &dataTarget{tbl: tbl, resource: tableName}
	}
	if err != nil {
		return nil, err
	}
	profile, err := s.resolveAppProfile(tableInstance(target.resource), appProfileID)
	if err != nil {
		return nil, err
	}
	target.appProfile = profile
	return target, nil
}

// resolveWriteTarget resolves mutation targets. transactional marks
// CheckAndMutateRow and ReadModifyWriteRow, which Bigtable allows only on
// single-cluster app profiles that permit transactional writes.
func (s *server) resolveWriteTarget(tableName, avName, appProfileID string, transactional bool) (*dataTarget, error) {
	if _, err := singleTarget(tableName, avName); err != nil {
		return nil, err
	}
	var target *dataTarget
	var err error
	if avName != "" {
		target, err = s.authorizedViewTarget(avName)
	} else {
		var tbl *table
		tbl, err = s.localRequireTable(tableName)
		target = &dataTarget{tbl: tbl, resource: tableName}
	}
	if err != nil {
		return nil, err
	}
	profile, err := s.resolveAppProfile(tableInstance(target.resource), appProfileID)
	if err != nil {
		return nil, err
	}
	if isDataBoostProfile(profile) {
		return nil, status.Errorf(codes.FailedPrecondition, "app profile %q uses Data Boost, which is read-only", profile.GetName())
	}
	if transactional && profile != nil {
		single := profile.GetSingleClusterRouting()
		if single == nil || !single.GetAllowTransactionalWrites() {
			return nil, status.Errorf(codes.FailedPrecondition,
				"app profile %q does not allow transactional writes; single-row transactions require single-cluster routing with allow_transactional_writes", profile.GetName())
		}
	}
	target.appProfile = profile
	return target, nil
}

func (s *server) authorizedViewTarget(avName string) (*dataTarget, error) {
	tableName := authorizedViewParentTable(avName)
	if tableName == avName {
		return nil, status.Errorf(codes.InvalidArgument, "invalid authorized view name %q", avName)
	}
	tbl, err := s.localRequireTable(tableName)
	if err != nil {
		return nil, err
	}
	av, ok, err := s.avBackend.Get(avName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "authorized view %q not found", avName)
	}
	return &dataTarget{tbl: tbl, view: compileViewPolicy(av), resource: avName}, nil
}

// viewPolicy is a compiled AuthorizedView.SubsetView.
type viewPolicy struct {
	name        string
	rowPrefixes []string
	families    map[string]*familySubset
}

type familySubset struct {
	qualifiers map[string]bool
	prefixes   []string
}

func compileViewPolicy(av *btapb.AuthorizedView) *viewPolicy {
	p := &viewPolicy{name: av.GetName(), families: make(map[string]*familySubset)}
	sv := av.GetSubsetView()
	for _, prefix := range sv.GetRowPrefixes() {
		p.rowPrefixes = append(p.rowPrefixes, string(prefix))
	}
	for fam, fs := range sv.GetFamilySubsets() {
		sub := &familySubset{qualifiers: make(map[string]bool)}
		for _, q := range fs.GetQualifiers() {
			sub.qualifiers[string(q)] = true
		}
		for _, q := range fs.GetQualifierPrefixes() {
			sub.prefixes = append(sub.prefixes, string(q))
		}
		p.families[fam] = sub
	}
	return p
}

func (p *viewPolicy) rowAllowed(key string) bool {
	for _, prefix := range p.rowPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func (p *viewPolicy) cellAllowed(fam, col string) bool {
	sub, ok := p.families[fam]
	if !ok {
		return false
	}
	if sub.qualifiers[col] {
		return true
	}
	for _, prefix := range sub.prefixes {
		if strings.HasPrefix(col, prefix) {
			return true
		}
	}
	return false
}

// familyFullyAllowed reports whether every qualifier of fam is in the view,
// which Bigtable requires for DeleteFromFamily through an authorized view.
func (p *viewPolicy) familyFullyAllowed(fam string) bool {
	sub, ok := p.families[fam]
	if !ok {
		return false
	}
	for _, prefix := range sub.prefixes {
		if prefix == "" {
			return true
		}
	}
	return false
}

// restrict removes every cell outside the view. Rows outside the view's row
// prefixes become empty.
func (p *viewPolicy) restrict(r *row) *row {
	out := newRow(r.key)
	if !p.rowAllowed(r.key) {
		return out
	}
	for _, fam := range r.families {
		for col, cs := range fam.Cells {
			if !p.cellAllowed(fam.Name, col) || len(cs) == 0 {
				continue
			}
			of := out.getOrCreateFamily(fam.Name, fam.Order)
			of.Cells[col] = append([]cell(nil), cs...)
		}
	}
	out.normalize()
	return out
}

func viewDenied(view, what string) error {
	return status.Errorf(codes.PermissionDenied, "request references %s outside authorized view %q", what, view)
}

// checkMutations rejects any mutation that touches data outside the view.
func (p *viewPolicy) checkMutations(key string, muts []*btpb.Mutation) error {
	if !p.rowAllowed(key) {
		return viewDenied(p.name, "row "+quoteKey(key))
	}
	for _, mut := range muts {
		switch m := mut.GetMutation().(type) {
		case *btpb.Mutation_SetCell_:
			if !p.cellAllowed(m.SetCell.FamilyName, string(m.SetCell.ColumnQualifier)) {
				return viewDenied(p.name, "column "+m.SetCell.FamilyName+":"+string(m.SetCell.ColumnQualifier))
			}
		case *btpb.Mutation_DeleteFromColumn_:
			if !p.cellAllowed(m.DeleteFromColumn.FamilyName, string(m.DeleteFromColumn.ColumnQualifier)) {
				return viewDenied(p.name, "column "+m.DeleteFromColumn.FamilyName+":"+string(m.DeleteFromColumn.ColumnQualifier))
			}
		case *btpb.Mutation_AddToCell_:
			col, _ := valueQualifier(m.AddToCell.ColumnQualifier)
			if !p.cellAllowed(m.AddToCell.FamilyName, col) {
				return viewDenied(p.name, "column "+m.AddToCell.FamilyName+":"+col)
			}
		case *btpb.Mutation_MergeToCell_:
			col, _ := valueQualifier(m.MergeToCell.ColumnQualifier)
			if !p.cellAllowed(m.MergeToCell.FamilyName, col) {
				return viewDenied(p.name, "column "+m.MergeToCell.FamilyName+":"+col)
			}
		case *btpb.Mutation_DeleteFromFamily_:
			if !p.familyFullyAllowed(m.DeleteFromFamily.FamilyName) {
				return viewDenied(p.name, "family "+m.DeleteFromFamily.FamilyName+" (DeleteFromFamily needs qualifier_prefixes \"\")")
			}
		case *btpb.Mutation_DeleteFromRow_:
			return status.Errorf(codes.PermissionDenied, "DeleteFromRow is not supported through authorized view %q", p.name)
		}
	}
	return nil
}

func (p *viewPolicy) checkRules(key string, rules []*btpb.ReadModifyWriteRule) error {
	if !p.rowAllowed(key) {
		return viewDenied(p.name, "row "+quoteKey(key))
	}
	for _, rule := range rules {
		if !p.cellAllowed(rule.GetFamilyName(), string(rule.GetColumnQualifier())) {
			return viewDenied(p.name, "column "+rule.GetFamilyName()+":"+string(rule.GetColumnQualifier()))
		}
	}
	return nil
}

func quoteKey(k string) string {
	return "\"" + k + "\""
}
