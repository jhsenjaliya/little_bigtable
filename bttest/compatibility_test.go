package bttest

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func TestCompatibilityLedgerIsValid(t *testing.T) {
	validLevels := map[CompatibilitySupportLevel]bool{
		CompatibilitySupported:             true,
		CompatibilityLocallySimplified:     true,
		CompatibilityExplicitlyUnsupported: true,
		CompatibilityNotApplicable:         true,
	}
	validVerification := map[CompatibilityVerification]bool{
		CompatibilityTestVerified:       true,
		CompatibilityKnownNonconformant: true,
		CompatibilityDeclaredUnverified: true,
	}
	seen := make(map[string]struct{})
	for _, capability := range CompatibilityLedger() {
		if capability.ID == "" {
			t.Fatal("capability ID is empty")
		}
		if !strings.HasPrefix(capability.ID, "rpc.") && !strings.HasPrefix(capability.ID, "field.") {
			t.Errorf("capability %q has an invalid ID prefix", capability.ID)
		}
		if _, ok := seen[capability.ID]; ok {
			t.Errorf("duplicate capability ID %q", capability.ID)
		}
		seen[capability.ID] = struct{}{}
		if !validLevels[capability.Support] {
			t.Errorf("capability %q has invalid support level %q", capability.ID, capability.Support)
		}
		if !validVerification[capability.Verification] {
			t.Errorf("capability %q has invalid verification state %q", capability.ID, capability.Verification)
		}
		if capability.LocalContract == "" {
			t.Errorf("capability %q has no local contract", capability.ID)
		}
		if capability.Observed == "" {
			t.Errorf("capability %q has no observed behavior", capability.ID)
		}
		if capability.OwningTest == "" {
			t.Errorf("capability %q has no owning test", capability.ID)
		}
		if capability.Verification == CompatibilityTestVerified && strings.HasPrefix(capability.OwningTest, "TestCompatibilityLedger") {
			t.Errorf("test-verified capability %q is owned only by ledger structure test %q", capability.ID, capability.OwningTest)
		}
	}
}

func TestCompatibilityLedgerReturnsCopy(t *testing.T) {
	first := CompatibilityLedger()
	if len(first) == 0 {
		t.Fatal("compatibility ledger is empty")
	}
	want := first[0]
	first[0].ID = "mutated"
	if got := CompatibilityLedger()[0]; got != want {
		t.Fatalf("CompatibilityLedger returned shared state: got %+v, want %+v", got, want)
	}
}

func TestCompatibilityLedgerVerifiedOwningTestsExist(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate compatibility test source")
	}
	testFiles, err := filepath.Glob(filepath.Join(filepath.Dir(sourceFile), "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	available := make(map[string]bool)
	fileSet := token.NewFileSet()
	for _, testFile := range testFiles {
		parsed, err := parser.ParseFile(fileSet, testFile, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", testFile, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") {
				available[function.Name.Name] = true
			}
		}
	}

	for _, capability := range CompatibilityLedger() {
		if capability.Verification == CompatibilityTestVerified && !available[capability.OwningTest] {
			t.Errorf("test-verified capability %q references missing test %q", capability.ID, capability.OwningTest)
		}
	}
}

func TestCompatibilityLedgerFieldIdentifiersExist(t *testing.T) {
	for _, capability := range CompatibilityLedger() {
		if !strings.HasPrefix(capability.ID, "field.") {
			continue
		}
		identifier := strings.TrimPrefix(capability.ID, "field.")
		separator := strings.LastIndexByte(identifier, '.')
		if separator < 1 || separator == len(identifier)-1 {
			t.Errorf("capability %q is not a message-field identifier", capability.ID)
			continue
		}
		messageName, fieldName := identifier[:separator], identifier[separator+1:]
		descriptor, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(messageName))
		if err != nil {
			t.Errorf("capability %q references an unknown message: %v", capability.ID, err)
			continue
		}
		message, ok := descriptor.(protoreflect.MessageDescriptor)
		if !ok {
			t.Errorf("capability %q does not reference a message", capability.ID)
			continue
		}
		if field := message.Fields().ByName(protoreflect.Name(fieldName)); field == nil {
			t.Errorf("capability %q references an unknown field", capability.ID)
		}
	}
}

func TestCompatibilityLedgerFieldsAreExhaustive(t *testing.T) {
	trackedMessages := map[protoreflect.FullName]bool{
		"google.bigtable.v2.ReadRowsRequest":           true,
		"google.bigtable.v2.SampleRowKeysRequest":      true,
		"google.bigtable.v2.MutateRowRequest":          true,
		"google.bigtable.v2.MutateRowsRequest":         true,
		"google.bigtable.v2.CheckAndMutateRowRequest":  true,
		"google.bigtable.v2.ReadModifyWriteRowRequest": true,
		"google.bigtable.admin.v2.CreateTableRequest":  true,
		"google.bigtable.admin.v2.ColumnFamily":        true,
	}

	var descriptorFields []string
	for messageName := range trackedMessages {
		descriptor, err := protoregistry.GlobalFiles.FindDescriptorByName(messageName)
		if err != nil {
			t.Fatalf("find tracked message %q: %v", messageName, err)
		}
		message := descriptor.(protoreflect.MessageDescriptor)
		for i := 0; i < message.Fields().Len(); i++ {
			descriptorFields = append(descriptorFields, fieldCapabilityID(string(messageName), string(message.Fields().Get(i).Name())))
		}
	}
	sort.Strings(descriptorFields)

	var declaredFields []string
	for _, capability := range CompatibilityLedger() {
		if !strings.HasPrefix(capability.ID, "field.") {
			continue
		}
		identifier := strings.TrimPrefix(capability.ID, "field.")
		separator := strings.LastIndexByte(identifier, '.')
		if separator > 0 && trackedMessages[protoreflect.FullName(identifier[:separator])] {
			declaredFields = append(declaredFields, capability.ID)
		}
	}
	sort.Strings(declaredFields)

	if strings.Join(descriptorFields, "\n") != strings.Join(declaredFields, "\n") {
		t.Fatalf("tracked message fields and compatibility ledger differ\ndescriptor only: %v\ndeclared only: %v",
			difference(descriptorFields, declaredFields), difference(declaredFields, descriptorFields))
	}
}

func TestCompatibilityLedgerCoversRegisteredRPCs(t *testing.T) {
	previousDialect, previousStrict := currentDialect(), isStrictAdmin()
	ConfigureStorage("sqlite3", true)
	t.Cleanup(func() { ConfigureStorage(string(previousDialect), previousStrict) })

	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=shared", newDBFile(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if err := CreateTables(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer("127.0.0.1:0", db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	var registered []string
	for service, info := range srv.srv.GetServiceInfo() {
		for _, method := range info.Methods {
			registered = append(registered, rpcCapabilityID(service, method.Name))
		}
	}
	sort.Strings(registered)

	var declared []string
	for _, capability := range CompatibilityLedger() {
		if strings.HasPrefix(capability.ID, "rpc.") {
			declared = append(declared, capability.ID)
		}
	}
	sort.Strings(declared)

	if strings.Join(registered, "\n") != strings.Join(declared, "\n") {
		t.Fatalf("registered RPCs and compatibility ledger differ\nregistered only: %v\ndeclared only: %v",
			difference(registered, declared), difference(declared, registered))
	}
}

func difference(left, right []string) []string {
	rightSet := make(map[string]struct{}, len(right))
	for _, item := range right {
		rightSet[item] = struct{}{}
	}
	var result []string
	for _, item := range left {
		if _, ok := rightSet[item]; !ok {
			result = append(result, item)
		}
	}
	return result
}
