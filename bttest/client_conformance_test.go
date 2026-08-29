package bttest

import (
	"context"
	"database/sql"
	"debug/buildinfo"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

const (
	cbtBinaryEnv          = "CBT_BIN"
	cbtRequiredEnv        = "CBT_REQUIRED"
	cbtExpectedVersionEnv = "CBT_EXPECTED_VERSION"
)

func TestCBTClientConformance(t *testing.T) {
	cbtPath := os.Getenv(cbtBinaryEnv)
	if cbtPath == "" {
		var err error
		cbtPath, err = exec.LookPath("cbt")
		if err != nil {
			if os.Getenv(cbtRequiredEnv) == "true" {
				t.Fatalf("cbt is required but was not found on PATH: %v", err)
			}
			t.Skip("cbt is not installed; CI provisions the pinned conformance version")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	versionOutput, err := exec.CommandContext(ctx, cbtPath, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("read cbt version: %v\n%s", err, versionOutput)
	}
	version := strings.TrimSpace(string(versionOutput))
	if expected := os.Getenv(cbtExpectedVersionEnv); expected != "" {
		info, err := buildinfo.ReadFile(cbtPath)
		if err != nil {
			t.Fatalf("read cbt build information: %v", err)
		}
		if info.Main.Path != "cloud.google.com/go/cbt" || info.Main.Version != expected {
			t.Fatalf("unexpected cbt build: got %s@%s, want cloud.google.com/go/cbt@%s",
				info.Main.Path, info.Main.Version, expected)
		}
		version = expected + " (binary reports " + version + ")"
	}
	t.Logf("cbt conformance version: %s", version)

	previousDialect, previousStrict := currentDialect(), isStrictAdmin()
	ConfigureStorage("sqlite3", true)
	t.Cleanup(func() { ConfigureStorage(string(previousDialect), previousStrict) })

	dbPath := filepath.Join(t.TempDir(), "cbt-conformance.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?cache=shared")
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
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	commandEnv := environmentWithOverrides(os.Environ(), map[string]string{
		"BIGTABLE_EMULATOR_HOST": srv.Addr,
	},
		"HOME",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_PROJECT",
		"GCLOUD_PROJECT",
		"CLOUDSDK_CONFIG",
		"CLOUDSDK_CORE_PROJECT",
	)
	commandDir := t.TempDir()

	const (
		projectID  = "cbt-conformance-project"
		instanceID = "cbt-conformance-instance"
		clusterID  = "cbt-conformance-cluster"
		tableID    = "cbt-conformance-table"
		family     = "cf"
	)
	baseArgs := []string{
		"-project=" + projectID,
		"-instance=" + instanceID,
		"-creds=",
		"-admin-endpoint=",
		"-data-endpoint=",
		"-cert-file=",
		"-auth-token=",
		"-access-token=emulator-conformance",
		"-timeout=10s",
	}
	runCBT := func(args ...string) string {
		t.Helper()
		cmdArgs := append(append([]string{}, baseArgs...), args...)
		cmd := exec.CommandContext(ctx, cbtPath, cmdArgs...)
		cmd.Env = commandEnv
		cmd.Dir = commandDir
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cbt %s failed with %s: %v\n%s", strings.Join(args, " "), version, err, output)
		}
		return string(output)
	}

	runCBT("createinstance", instanceID, "CBT Conformance", clusterID, "us-central1-b", "1", "SSD")
	runCBT("createtable", tableID)
	runCBT("createfamily", tableID, family)
	runCBT("set", tableID, "row-a", family+":value=outside-before")
	runCBT("set", tableID, "row-b", family+":value=inside-range")
	runCBT("set", tableID, "row-c", family+":value=outside-after")

	boundedRead := runCBT("read", tableID, "start=row-b", "end=row-c")
	if !strings.Contains(boundedRead, "row-b") || !strings.Contains(boundedRead, "inside-range") {
		t.Fatalf("cbt bounded read with %s omitted the expected row:\n%s", version, boundedRead)
	}
	for _, outside := range []string{"row-a", "outside-before", "row-c", "outside-after"} {
		if strings.Contains(boundedRead, outside) {
			t.Fatalf("cbt bounded read with %s included out-of-range value %q:\n%s", version, outside, boundedRead)
		}
	}

	runCBT("deletetable", tableID)
	if tables := runCBT("ls"); strings.Contains(tables, tableID) {
		t.Fatalf("cbt deletetable with %s left %q visible:\n%s", version, tableID, tables)
	}
	runCBT("deleteinstance", instanceID)
	if instances := runCBT("listinstances"); strings.Contains(instances, instanceID) {
		t.Fatalf("cbt deleteinstance with %s left %q visible:\n%s", version, instanceID, instances)
	}
}

func environmentWithOverrides(base []string, overrides map[string]string, removals ...string) []string {
	removed := make(map[string]bool, len(removals))
	for _, name := range removals {
		removed[name] = true
	}
	environment := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			if removed[name] {
				continue
			}
			if _, overridden := overrides[name]; overridden {
				continue
			}
		}
		environment = append(environment, entry)
	}
	for name, value := range overrides {
		environment = append(environment, fmt.Sprintf("%s=%s", name, value))
	}
	return environment
}
