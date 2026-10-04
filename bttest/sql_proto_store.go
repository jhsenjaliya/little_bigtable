package bttest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// protoStore persists protobuf resources in a SQL table with a name primary
// key, an optional parent column, and a metadata blob.
type protoStore[T proto.Message] struct {
	db        *sql.DB
	table     string
	parentCol string // empty when the table has no parent column
	newT      func() T
}

func (p *protoStore[T]) exec(q sqlExecutor) sqlExecutor {
	if q == nil {
		return p.db
	}
	return q
}

func (p *protoStore[T]) get(ctx context.Context, q sqlExecutor, name string) (T, bool, error) {
	var zero T
	var data []byte
	err := p.exec(q).QueryRowContext(ctx, bind("SELECT metadata FROM "+p.table+" WHERE name = ?"), name).Scan(&data)
	if err == sql.ErrNoRows {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, fmt.Errorf("load %s: %w", name, err)
	}
	msg := p.newT()
	if err := proto.Unmarshal(data, msg); err != nil {
		return zero, false, fmt.Errorf("decode %s: %w", name, err)
	}
	return msg, true, nil
}

func (p *protoStore[T]) put(ctx context.Context, q sqlExecutor, name, parent string, msg T) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	if p.parentCol == "" {
		_, err = p.exec(q).ExecContext(ctx,
			bind("INSERT INTO "+p.table+" (name, metadata) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET metadata = ?"),
			name, data, data)
	} else {
		_, err = p.exec(q).ExecContext(ctx,
			bind("INSERT INTO "+p.table+" (name, "+p.parentCol+", metadata) VALUES (?, ?, ?) ON CONFLICT (name) DO UPDATE SET "+p.parentCol+" = ?, metadata = ?"),
			name, parent, data, parent, data)
	}
	if err != nil {
		return fmt.Errorf("save %s: %w", name, err)
	}
	return nil
}

func (p *protoStore[T]) remove(ctx context.Context, q sqlExecutor, name string) error {
	if _, err := p.exec(q).ExecContext(ctx, bind("DELETE FROM "+p.table+" WHERE name = ?"), name); err != nil {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	return nil
}

// listPrefix returns resources whose name starts with prefix, ordered by name.
func (p *protoStore[T]) listPrefix(ctx context.Context, q sqlExecutor, prefix string) ([]T, error) {
	rows, err := p.exec(q).QueryContext(ctx, bind("SELECT name, metadata FROM "+p.table+" WHERE name LIKE ? ORDER BY name"), likePrefix(prefix))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", p.table, err)
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var name string
		var data []byte
		if err := rows.Scan(&name, &data); err != nil {
			return nil, fmt.Errorf("list %s: %w", p.table, err)
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		msg := p.newT()
		if err := proto.Unmarshal(data, msg); err != nil {
			return nil, fmt.Errorf("decode %s: %w", name, err)
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

func (p *protoStore[T]) removePrefix(ctx context.Context, q sqlExecutor, prefix string) error {
	items, err := p.listNames(ctx, q, prefix)
	if err != nil {
		return err
	}
	for _, name := range items {
		if err := p.remove(ctx, q, name); err != nil {
			return err
		}
	}
	return nil
}

func (p *protoStore[T]) listNames(ctx context.Context, q sqlExecutor, prefix string) ([]string, error) {
	rows, err := p.exec(q).QueryContext(ctx, bind("SELECT name FROM "+p.table+" WHERE name LIKE ?"), likePrefix(prefix))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", p.table, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names, rows.Err()
}

// contentEtag derives a resource etag from its content (excluding the etag
// field), so it changes exactly when the resource changes.
func contentEtag(msg proto.Message) string {
	clone := proto.Clone(msg)
	fd := clone.ProtoReflect().Descriptor().Fields().ByName("etag")
	if fd != nil {
		clone.ProtoReflect().Clear(fd)
	}
	data, _ := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	sum := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}

// checkEtag returns ABORTED when a caller-supplied etag is stale.
func checkEtag(requested, current string) error {
	if requested != "" && requested != current {
		return status.Errorf(codes.Aborted, "etag %q does not match the current etag %q", requested, current)
	}
	return nil
}

func internalErr(err error) error {
	return storageErr(err)
}
