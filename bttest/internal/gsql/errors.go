package gsql

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Error conventions used throughout the package:
//
//   - InvalidArgument: syntax, type and semantic errors, and bad parameter values.
//   - NotFound: unknown tables and views.
//   - Unimplemented: valid GoogleSQL for Bigtable that the emulator does not support.
//   - OutOfRange: runtime evaluation errors (division by zero, overflow, invalid
//     casts, out-of-bounds array access, ...), matching ZetaSQL conventions.
//     These are the errors that SAFE_ functions, SAFE_CAST, IFERROR and friends
//     convert into NULL / fallback values.

func invalidf(format string, a ...any) error {
	return status.Error(codes.InvalidArgument, fmt.Sprintf(format, a...))
}

func unimplementedf(format string, a ...any) error {
	return status.Error(codes.Unimplemented, fmt.Sprintf(format, a...))
}

func notFoundf(format string, a ...any) error {
	return status.Error(codes.NotFound, fmt.Sprintf(format, a...))
}

func evalErrorf(format string, a ...any) error {
	return status.Error(codes.OutOfRange, fmt.Sprintf(format, a...))
}

func internalf(format string, a ...any) error {
	return status.Error(codes.Internal, fmt.Sprintf(format, a...))
}

// isEvalError reports whether err is a runtime evaluation error that can be
// absorbed by SAFE_ variants, IFERROR, NULLIFERROR and ISERROR.
func isEvalError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch status.Code(err) {
	case codes.OutOfRange, codes.InvalidArgument:
		return true
	}
	return false
}

// toStatus converts arbitrary errors (e.g. from a Catalog) into gRPC status errors.
func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Internal, err.Error())
}

func statusMessage(err error) string { return status.Convert(err).Message() }

func sortStrings(s []string) { sort.Strings(s) }
